package timewheel

import (
	"strings"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		attempt int
		claimID string
		payload string
	}{
		{"normal", "order:1001", 3, "batch-1:2", `{"key":"order:1001","msg":"close","type":"order","executeAtUnix":1700000000}`},
		{"zero_attempt_empty_claim", "k1", 0, "", `{"key":"k1","msg":"m","type":"t","executeAtUnix":1}`},
		{"empty_key", "", 1, "b:1", `{"msg":"m","type":"t"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			member := encodeEnvelope(tt.key, tt.attempt, tt.claimID, []byte(tt.payload))
			env, err := decodeEnvelope(member)
			if err != nil {
				t.Fatalf("decodeEnvelope: %v", err)
			}
			if env.Legacy {
				t.Fatalf("expected non-legacy envelope")
			}
			if env.Key != tt.key {
				t.Errorf("key: got %q, want %q", env.Key, tt.key)
			}
			if env.Attempt != tt.attempt {
				t.Errorf("attempt: got %d, want %d", env.Attempt, tt.attempt)
			}
			if env.ClaimID != tt.claimID {
				t.Errorf("claimID: got %q, want %q", env.ClaimID, tt.claimID)
			}
			if string(env.Payload) != tt.payload {
				t.Errorf("payload: got %q, want %q", env.Payload, tt.payload)
			}
		})
	}
}

func TestEnvelopeKeyWithPipeAndChinese(t *testing.T) {
	key := "a|b/c"
	payload := `{"key":"a|b/c","msg":"中文消息-订单超时","type":"订单","executeAtUnix":1700000000}`
	member := encodeEnvelope(key, 2, "uuid:9", []byte(payload))
	env, err := decodeEnvelope(member)
	if err != nil {
		t.Fatalf("decodeEnvelope: %v", err)
	}
	if env.Key != key {
		t.Errorf("key: got %q, want %q", env.Key, key)
	}
	if string(env.Payload) != payload {
		t.Errorf("payload mismatch: got %q", env.Payload)
	}
	if !strings.Contains(string(env.Payload), "中文消息") {
		t.Errorf("chinese payload should not be escaped or altered, got %q", env.Payload)
	}
}

func TestDecodeLegacyJSONMember(t *testing.T) {
	legacy := `{"key":"k1","msg":"m","type":"t","executeAtUnix":1700000000}`
	env, err := decodeEnvelope(legacy)
	if err != nil {
		t.Fatalf("decodeEnvelope: %v", err)
	}
	if !env.Legacy {
		t.Fatalf("expected legacy envelope for raw JSON member")
	}
	if string(env.Payload) != legacy {
		t.Errorf("legacy payload should be the raw member, got %q", env.Payload)
	}
	if env.Key != "" || env.Attempt != 0 || env.ClaimID != "" {
		t.Errorf("legacy envelope fields should be zero values, got %+v", env)
	}
}

func TestDecodeMalformed(t *testing.T) {
	bad := []string{
		"",                                  // empty
		"v1\x1fk1\x1f1\x1fc1",               // 4 segments
		"v2\x1fk1\x1f1\x1fc1\x1f{}",         // wrong version
		"v1\x1fk1\x1fabc\x1fc1\x1f{}",       // non-numeric attempt
		"v1\x1fk1\x1f1\x1fc1\x1f{}\x1fextra", // split with 5 cuts -> handled by SplitN(5), payload retains extra sep? must not error on extra sep
	}
	// note: the last case is actually valid per SplitN(5) semantics; assert it decodes with payload containing the remainder
	for i, m := range bad {
		env, err := decodeEnvelope(m)
		if i == 4 {
			if err != nil {
				t.Errorf("case %d (%q): unexpected error %v", i, m, err)
			}
			if !strings.Contains(string(env.Payload), "\x1fextra") {
				t.Errorf("case %d: payload should keep remainder after 5th sep, got %q", i, env.Payload)
			}
			continue
		}
		if err == nil {
			t.Errorf("case %d (%q): expected error, got env %+v", i, m, env)
		}
	}
}

func TestDecodeTaskPayload(t *testing.T) {
	task, err := decodeTaskPayload([]byte(`{"key":"k1","msg":"m","type":"t","executeAtUnix":123}`))
	if err != nil {
		t.Fatalf("decodeTaskPayload: %v", err)
	}
	if task.Key != "k1" || task.Msg != "m" || task.Type != "t" || task.ExecuteAtUnix != 123 {
		t.Errorf("task fields mismatch: %+v", task)
	}

	if _, err := decodeTaskPayload([]byte(`{not json`)); err == nil {
		t.Errorf("expected error for malformed JSON payload")
	}
}
