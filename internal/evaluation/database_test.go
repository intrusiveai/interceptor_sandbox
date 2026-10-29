package evaluation

import (
	"encoding/json"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

func TestDatabaseOracleRecordedEvaluation(t *testing.T) {
	for _, tc := range []struct {
		name, detail       string
		fired, unavailable bool
	}{
		{"committed", `{"source":"application","matched":true,"result":[[1]]}`, true, false},
		{"nonmatching", `{"source":"application","matched":false,"result":[[0]]}`, false, false},
		{"empty_rows", `{"source":"application","matched":false,"result":[]}`, false, false},
		{"seeded", `{"source":"baseline","matched":true,"result":[[1]]}`, false, false},
		{"harness", `{"source":"harness","matched":true,"result":[[1]]}`, false, false},
		{"missing_match", `{"source":"application","result":[[1]]}`, false, true},
		{"null_match", `{"source":"application","matched":null,"result":[[1]]}`, false, true},
		{"missing_result", `{"source":"application","matched":false}`, false, true},
		{"invalid_rows", `{"source":"application","matched":false,"result":{}}`, false, true},
		{"missing_source", `{"matched":false,"result":[[1]]}`, false, true},
		{"unknown_source", `{"source":"unknown","matched":true,"result":[[1]]}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var detail map[string]any
			if err := json.Unmarshal([]byte(tc.detail), &detail); err != nil {
				t.Fatal(err)
			}
			detail["oracle"] = "refund"
			raw, _ := json.Marshal(map[string]any{"service": "database", "detail": detail})
			events := []recording.Event{{Seq: 7, Hash: "retained-hash", Kind: "database_oracle", Semantic: raw}}
			report, err := Evaluate(Input{Blueprint: &environment.Blueprint{Oracles: []environment.Oracle{{ID: "refund", Kind: "database_query_match", Service: "database"}}}, Events: events})
			if (err != nil) != tc.unavailable {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if err != nil {
				return
			}
			result := report.Oracles[0]
			if result.Fired != tc.fired || len(result.CausalIDs) != 0 {
				t.Fatal(result)
			}
			if tc.fired && (result.FirstEventSeq != 7 || result.FirstEventDigest != "retained-hash") {
				t.Fatal(result)
			}
		})
	}
}
