package simnet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

const maximumDNSMessage = 4096

type DNSServer struct {
	udp           net.PacketConn
	tcp           net.Listener
	records       map[string]struct{}
	policy        string
	limit         int64
	recorder      *recording.Writer
	queries       atomic.Int64
	limitRecorded atomic.Bool
	closed        chan struct{}
	once          sync.Once
	wg            sync.WaitGroup
}

func StartDNS(address string, hostnames []string, unknownPolicy string, maximumQueries int64, recorder *recording.Writer) (*DNSServer, error) {
	records := make(map[string]struct{}, len(hostnames))
	for _, hostname := range hostnames {
		records[normalizeHostname(hostname)] = struct{}{}
	}
	udp, err := net.ListenPacket("udp", address)
	if err != nil {
		return nil, err
	}
	tcp, err := net.Listen("tcp", address)
	if err != nil {
		udp.Close()
		return nil, err
	}
	server := &DNSServer{udp: udp, tcp: tcp, records: records, policy: unknownPolicy, limit: maximumQueries, recorder: recorder, closed: make(chan struct{})}
	server.wg.Add(2)
	go server.serveUDP()
	go server.serveTCP()
	return server, nil
}

func (s *DNSServer) Close() error {
	var result error
	s.once.Do(func() {
		close(s.closed)
		result = errors.Join(s.udp.Close(), s.tcp.Close())
		s.wg.Wait()
	})
	return result
}

func (s *DNSServer) serveUDP() {
	defer s.wg.Done()
	buffer := make([]byte, maximumDNSMessage)
	for {
		count, address, err := s.udp.ReadFrom(buffer)
		if err != nil {
			return
		}
		response, err := s.answer(buffer[:count])
		if err == nil {
			_, _ = s.udp.WriteTo(response, address)
		}
	}
}

func (s *DNSServer) serveTCP() {
	defer s.wg.Done()
	semaphore := make(chan struct{}, 16)
	for {
		connection, err := s.tcp.Accept()
		if err != nil {
			return
		}
		semaphore <- struct{}{}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-semaphore }()
			s.handleTCP(connection)
		}()
	}
}

func (s *DNSServer) handleTCP(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	var length [2]byte
	if _, err := io.ReadFull(connection, length[:]); err != nil {
		return
	}
	size := int(binary.BigEndian.Uint16(length[:]))
	if size < 12 || size > maximumDNSMessage {
		return
	}
	query := make([]byte, size)
	if _, err := io.ReadFull(connection, query); err != nil {
		return
	}
	response, err := s.answer(query)
	if err != nil || len(response) > 65535 {
		return
	}
	binary.BigEndian.PutUint16(length[:], uint16(len(response)))
	_, _ = connection.Write(append(length[:], response...))
}

func (s *DNSServer) answer(query []byte) ([]byte, error) {
	if len(query) < 12 || binary.BigEndian.Uint16(query[4:6]) != 1 || binary.BigEndian.Uint16(query[2:4])&0xf800 != 0 {
		return nil, errors.New("DNS query must contain exactly one question")
	}
	name, questionEnd, err := parseQuestionName(query, 12)
	if err != nil || questionEnd+4 > len(query) {
		return nil, errors.New("invalid DNS question")
	}
	questionEnd += 4
	qtype := binary.BigEndian.Uint16(query[questionEnd-4 : questionEnd-2])
	qclass := binary.BigEndian.Uint16(query[questionEnd-2 : questionEnd])
	_, declared := s.records[name]
	rcode, outcome := uint16(0), "no-data"
	answers := uint16(0)
	limitExceeded := s.limit > 0 && s.queries.Add(1) > s.limit
	if limitExceeded {
		rcode, outcome = 5, "query-limit-exceeded"
	} else if !declared {
		outcome = s.policy
		if s.policy == "refused" {
			rcode = 5
		} else {
			rcode = 3
		}
	} else if qclass == 1 && (qtype == 1 || qtype == 255) {
		answers, outcome = 1, "loopback"
	}
	response := make([]byte, 12, 12+(questionEnd-12)+16)
	copy(response[0:2], query[0:2])
	flags := uint16(0x8400) | binary.BigEndian.Uint16(query[2:4])&0x0100 | rcode
	binary.BigEndian.PutUint16(response[2:4], flags)
	binary.BigEndian.PutUint16(response[4:6], 1)
	binary.BigEndian.PutUint16(response[6:8], answers)
	response = append(response, query[12:questionEnd]...)
	if answers == 1 {
		response = append(response, 0xc0, 0x0c, 0x00, 0x01, 0x00, 0x01)
		response = append(response, 0, 0, 0, 0, 0, 4, 127, 0, 0, 1)
	}
	if !limitExceeded || s.limitRecorded.CompareAndSwap(false, true) {
		s.record(name, qtype, outcome, declared)
	}
	return response, nil
}

func parseQuestionName(message []byte, offset int) (string, int, error) {
	labels := make([]string, 0, 8)
	length := 0
	for {
		if offset >= len(message) {
			return "", 0, io.ErrUnexpectedEOF
		}
		size := int(message[offset])
		offset++
		if size == 0 {
			break
		}
		if size > 63 || size&0xc0 != 0 || offset+size > len(message) {
			return "", 0, errors.New("compressed or invalid DNS question name")
		}
		label := string(message[offset : offset+size])
		if strings.ContainsAny(label, "\x00\r\n") {
			return "", 0, errors.New("invalid DNS label")
		}
		labels = append(labels, strings.ToLower(label))
		length += size + 1
		if length > 253 {
			return "", 0, errors.New("DNS name is too long")
		}
		offset += size
	}
	if len(labels) == 0 {
		return "", 0, errors.New("root DNS question is unsupported")
	}
	return strings.Join(labels, "."), offset, nil
}

func (s *DNSServer) record(hostname string, qtype uint16, outcome string, declared bool) {
	if s.recorder == nil {
		return
	}
	semantic := map[string]any{"hostname": hostname, "query_type": dnsType(qtype), "outcome": outcome, "declared": declared}
	_, _ = s.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "dns", Kind: "dns_query", Provenance: recording.Application, Semantic: semantic})
	if !declared {
		_, _ = s.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "dns", Kind: "coverage_gap", Provenance: recording.EnvironmentSealed, Semantic: semantic})
	}
}

func dnsType(value uint16) string {
	switch value {
	case 1:
		return "A"
	case 28:
		return "AAAA"
	case 255:
		return "ANY"
	default:
		return fmt.Sprintf("TYPE%d", value)
	}
}
