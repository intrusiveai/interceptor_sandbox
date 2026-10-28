package postgresql

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgproto3"
)

func decodeCell(f pgproto3.FieldDescription, b []byte) any {
	if b == nil {
		return nil
	}
	switch f.DataTypeOID {
	case 25, 1043, 1042:
		if utf8.Valid(b) {
			return string(b)
		}
	case 114, 3802:
		if f.DataTypeOID == 3802 && f.Format == 1 {
			if len(b) == 0 || b[0] != 1 {
				return map[string]any{"opaque": base64.StdEncoding.EncodeToString(b)}
			}
			b = b[1:]
		}
		var v any
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		if d.Decode(&v) == nil {
			return v
		}
	}
	return map[string]any{"opaque": base64.StdEncoding.EncodeToString(b)}
}
func encodeCell(f pgproto3.FieldDescription, original []byte, v any) ([]byte, error) {
	if reflect.DeepEqual(decodeCell(f, original), v) {
		return original, nil
	}
	if original == nil || v == nil {
		return nil, errors.New("database response placement cannot change NULL semantics")
	}
	switch f.DataTypeOID {
	case 25, 1043, 1042:
		text, ok := v.(string)
		if !ok || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return nil, errors.New("database text placement requires valid non-NUL UTF-8")
		}
		if (f.DataTypeOID == 1043 || f.DataTypeOID == 1042) && f.TypeModifier >= 4 && utf8.RuneCountInString(text) > int(f.TypeModifier-4) {
			return nil, errors.New("database text placement exceeds declared character limit")
		}
		return []byte(text), nil
	case 114, 3802:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if f.DataTypeOID == 3802 && f.Format == 1 {
			b = append([]byte{1}, b...)
		}
		return b, nil
	}
	return nil, errors.New("database response placement supports text and JSON/JSONB columns only")
}

// sqlTokens only identifies protocol-affecting commands and SQL cursor names;
// PostgreSQL remains the SQL parser. Quoted strings/comments never count as SQL
// keywords, and unterminated lexical forms fail before forwarding.
func sqlTokens(sql string) ([]string, error) {
	var tokens []string
	for i := 0; i < len(sql); {
		c := sql[i]
		if unicode.IsSpace(rune(c)) {
			i++
			continue
		}
		if c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			depth := 1
			i += 2
			for i < len(sql) && depth > 0 {
				if strings.HasPrefix(sql[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(sql[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return nil, errors.New("unterminated SQL comment")
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote := c
			i++
			var value strings.Builder
			closed := false
			for i < len(sql) {
				if sql[i] == quote {
					if i+1 < len(sql) && sql[i+1] == quote {
						value.WriteByte(quote)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				if quote == '\'' && sql[i] == '\\' && i+1 < len(sql) {
					i += 2
					continue
				}
				value.WriteByte(sql[i])
				i++
			}
			if !closed {
				return nil, errors.New("unterminated SQL quoted value")
			}
			if quote == '"' {
				tokens = append(tokens, "\""+value.String())
			} else {
				tokens = append(tokens, "'value")
			}
			continue
		}
		if c == '$' {
			j := i + 1
			for j < len(sql) && (unicode.IsLetter(rune(sql[j])) || unicode.IsDigit(rune(sql[j])) || sql[j] == '_') {
				j++
			}
			if j < len(sql) && sql[j] == '$' {
				tag := sql[i : j+1]
				end := strings.Index(sql[j+1:], tag)
				if end < 0 {
					return nil, errors.New("unterminated SQL dollar quote")
				}
				i = j + 1 + end + len(tag)
				tokens = append(tokens, "'value")
				continue
			}
		}
		j := i
		for i < len(sql) && (unicode.IsLetter(rune(sql[i])) || unicode.IsDigit(rune(sql[i])) || sql[i] == '_' || sql[i] == '$') {
			i++
		}
		if i == j {
			i++
		}
		tokens = append(tokens, sql[j:i])
	}
	return tokens, nil
}
func cursorName(t string) string {
	if strings.HasPrefix(t, "\"") {
		return strings.TrimPrefix(t, "\"")
	}
	return strings.ToLower(t)
}

type cursor struct {
	sql        string
	parameters *portal
}

func (s *connection) cursorQuery(sql string, execution ...*execution) string {
	t, err := sqlTokens(sql)
	if err != nil || len(t) == 0 {
		return sql
	}
	if strings.EqualFold(t[0], "DECLARE") && len(t) > 3 {
		var bound *portal
		if len(execution) > 0 {
			bound = execution[0].portal
		}
		s.cursors[cursorName(t[1])] = cursor{sql: sql, parameters: bound}
	}
	if strings.EqualFold(t[0], "FETCH") {
		for i := len(t) - 1; i >= 1; i-- {
			if q, ok := s.cursors[cursorName(t[i])]; ok {
				if len(execution) > 0 {
					execution[0].origin = q.parameters
				}
				return q.sql
			}
		}
	}
	if strings.EqualFold(t[0], "CLOSE") && len(t) > 1 {
		delete(s.cursors, cursorName(t[1]))
	}
	return sql
}
