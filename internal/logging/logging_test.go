package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

const (
	privateKey = "0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	address    = "0x52908400098527886e0f7030069857d2e4169ee7"
	bearer     = "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abcdef"
)

func TestRedactPrivateKey(t *testing.T) {
	out := Redact("key=" + privateKey)
	if strings.Contains(out, privateKey) {
		t.Errorf("private key leaked: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction marker: %q", out)
	}
}

func TestRedactAddress(t *testing.T) {
	out := Redact("to=" + address)
	if strings.Contains(out, address) {
		t.Errorf("address leaked: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction marker: %q", out)
	}
}

func TestRedactURLCredentials(t *testing.T) {
	out := Redact("upstream https://user:secret@rpc.example.com/rpc")
	if strings.Contains(out, "user:secret") {
		t.Errorf("url userinfo leaked: %q", out)
	}
	if !strings.Contains(out, "https://[REDACTED]@rpc.example.com") {
		t.Errorf("expected masked userinfo: %q", out)
	}
}

func TestRedactBearer(t *testing.T) {
	out := Redact(bearer)
	if strings.Contains(out, "eyJhbGci") {
		t.Errorf("bearer token leaked: %q", out)
	}
	if !strings.Contains(out, "Bearer [REDACTED]") {
		t.Errorf("expected masked bearer: %q", out)
	}
}

func TestRedactTokenAssignment(t *testing.T) {
	out := Redact("?token=abc123xyz&other=1")
	if strings.Contains(out, "abc123xyz") {
		t.Errorf("token value leaked: %q", out)
	}
}

func TestRedactDeterministic(t *testing.T) {
	if Redact(bearer) != Redact(bearer) {
		t.Error("Redact must be deterministic")
	}
}

func TestRedactLeavesSafeText(t *testing.T) {
	safe := `{"jsonrpc":"2.0","method":"eth_chainId"}`
	if out := Redact(safe); out != safe {
		t.Errorf("safe text must pass through unchanged: %q", out)
	}
}

func TestRedactRawTransactionPayload(t *testing.T) {
	// A raw tx is a 0x-prefixed hex run longer than any key or address:
	// 200 hex chars here. The whole run must vanish in one replacement.
	rawTx := "0x" + strings.Repeat("ab", 100)
	out := Redact(`{"jsonrpc":"2.0","method":"eth_sendRawTransaction","params":["` + rawTx + `"],"id":1}`)
	if strings.Contains(out, "abab") {
		t.Errorf("raw tx hex leaked: %q", out)
	}
	if !strings.Contains(out, "0x[REDACTED]") {
		t.Errorf("expected redaction marker: %q", out)
	}
}

func TestRedactLeavesShortHex(t *testing.T) {
	// Six hex chars are below the 8-char threshold and must pass through:
	// documents the boundary between benign short hex and sensitive runs.
	in := `{"data":"0x1a2b3c"}`
	if out := Redact(in); out != in {
		t.Errorf("short hex must pass through unchanged: %q", out)
	}
}

func TestLogRecordRedactsEthSendRawTransactionPayload(t *testing.T) {
	// Defense-in-depth: a full eth_sendRawTransaction payload carrying both
	// a raw tx and a private key must never land in the log (constitution V).
	var buf bytes.Buffer
	logger := NewWithOutput(&buf, slog.LevelDebug)
	rawTx := "0x" + strings.Repeat("cd", 100)
	payload := `{"jsonrpc":"2.0","method":"eth_sendRawTransaction","params":["` + rawTx + `","` + privateKey + `"],"id":1}`
	logger.Info("request", "payload", payload)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("logger output is not valid JSON: %v (%q)", err, buf.String())
	}
	out, _ := rec["payload"].(string)
	if strings.Contains(out, "cdcd") {
		t.Errorf("raw tx hex leaked into JSON log: %q", out)
	}
	if strings.Contains(out, privateKey[10:]) {
		t.Errorf("private key leaked into JSON log: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction marker: %q", out)
	}
}

func TestNewWithOutputRedactsRecords(t *testing.T) {
	var buf bytes.Buffer
	logger := NewWithOutput(&buf, slog.LevelDebug)
	logger.Info("request", "private_key", privateKey, "method", "eth_chainId")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("logger output is not valid JSON: %v (%q)", err, buf.String())
	}
	msg, _ := rec["msg"].(string)
	if msg != "request" {
		t.Errorf("msg: got %q", msg)
	}
	pk, _ := rec["private_key"].(string)
	if strings.Contains(pk, privateKey[10:]) {
		t.Errorf("private key leaked into JSON log: %q", pk)
	}
	if method, _ := rec["method"].(string); method != "eth_chainId" {
		t.Errorf("safe attribute must survive: %q", method)
	}
}
