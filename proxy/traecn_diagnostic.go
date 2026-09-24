package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Diagnostics are opt-in, local, and bounded across process restarts. Reuse a
// directory to keep the same capture quota; choose a new directory for a new run.
var traeCNDiagnosticMu sync.Mutex

type traeCNDiagnosticIngressKey struct{}
type traeCNDiagnosticContextKey struct{}
type traeCNDiagnosticIngress struct {
	body    []byte
	total   int
	summary map[string]any
}

type traeCNDiagnostic struct {
	id            string
	dir           string
	raw           bool
	limit         int
	secrets       []string
	mu            sync.Mutex
	metadataBytes int
	upstreamCount int
}

func traeCNDiagnosticConfig() (string, int, int, bool) {
	dir := strings.TrimSpace(os.Getenv("TRAECN_DIAGNOSTIC_DIR"))
	count, _ := strconv.Atoi(os.Getenv("TRAECN_DIAGNOSTIC_REQUESTS"))
	size, _ := strconv.Atoi(os.Getenv("TRAECN_DIAGNOSTIC_MAX_BYTES"))
	if size <= 0 {
		size = 1 << 20
	}
	size = min(size, 16<<20)
	return dir, min(max(count, 0), 20), size, os.Getenv("TRAECN_DIAGNOSTIC_RAW") == "1"
}

// Capture before validation, model mapping, context replay, or body preparation.
// Keep the original request distinct from the executor's canonical input.
func captureTraeCNDiagnosticIngress(c *gin.Context, body []byte) {
	dir, count, size, raw := traeCNDiagnosticConfig()
	if dir == "" || count == 0 || c.Request == nil {
		return
	}
	body = ingressRequestBody(c, body)
	snapshot := &traeCNDiagnosticIngress{total: len(body), summary: traeCNDiagnosticRequestSummary(body)}
	snapshot.summary["gateway_request_id"] = ensurePromptPolicyRequestCorrelationID(c)
	if raw {
		snapshot.body = bytes.Clone(body[:min(len(body), size)])
	}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), traeCNDiagnosticIngressKey{}, snapshot))
}

func newTraeCNDiagnostic(ctx context.Context, id string, fallback []byte, headers http.Header, secrets ...string) *traeCNDiagnostic {
	d := &traeCNDiagnostic{id: id, secrets: secrets}
	dir, count, size, raw := traeCNDiagnosticConfig()
	if dir == "" || count == 0 {
		return d
	}
	traeCNDiagnosticMu.Lock()
	defer traeCNDiagnosticMu.Unlock()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return d
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return d
	}
	used := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "traecn-") {
			used++
		}
	}
	if used >= count {
		return d
	}
	captureDir := filepath.Join(dir, "traecn-"+id)
	if err := os.Mkdir(captureDir, 0700); err != nil {
		return d
	}
	d.dir, d.limit, d.raw = captureDir, size, raw
	// No headers are written. Also remove their credential values if echoed in a body.
	for key, values := range headers {
		k := strings.ToLower(key)
		if strings.Contains(k, "token") || strings.Contains(k, "key") || strings.Contains(k, "auth") || strings.Contains(k, "cookie") {
			for _, value := range values {
				d.secrets = append(d.secrets, value)
				if strings.HasPrefix(value, "Bearer ") {
					d.secrets = append(d.secrets, strings.TrimPrefix(value, "Bearer "))
				}
			}
		}
	}
	snapshot, _ := ctx.Value(traeCNDiagnosticIngressKey{}).(*traeCNDiagnosticIngress)
	if snapshot != nil {
		d.record("ingress", snapshot.summary)
		d.artifact("inbound.json", snapshot.body, snapshot.total)
	} else {
		// Direct executor calls (including probes) have no HTTP ingress snapshot.
		d.record("executor_input", traeCNDiagnosticRequestSummary(fallback))
		d.artifact("executor-input.json", fallback, len(fallback))
	}
	log.Printf("[TRAECN] stage=diagnostic request_id=%q raw=%t", id, raw)
	return d
}

var traeCNDiagnosticCredential = regexp.MustCompile(`(?i)("(?:authorization|proxy-authorization|x-cloudide-token|x-ide-token|x-api-key|api_key|access_token|refresh_token|cookie|set-cookie)"\s*:\s*")[^"\r\n]*("|$)`)

func (d *traeCNDiagnostic) redact(body []byte) []byte {
	text := string(body)
	for _, secret := range d.secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
		// A capped artifact can end in the middle of a credential.
		for n := min(len(secret)-1, len(text)); n >= 4; n-- {
			if strings.HasSuffix(text, secret[:n]) {
				text = text[:len(text)-n] + "[REDACTED]"
				break
			}
		}
	}
	return []byte(traeCNDiagnosticCredential.ReplaceAllString(text, "${1}[REDACTED]${2}"))
}

func (d *traeCNDiagnostic) record(stage string, value any) {
	if d == nil || d.dir == "" {
		return
	}
	raw, err := json.Marshal(map[string]any{"request_id": d.id, "stage": stage, "data": value})
	if err != nil {
		return
	}
	raw = append(d.redact(raw), '\n')
	d.mu.Lock()
	defer d.mu.Unlock()
	// Preserve the terminal observation even if a long SSE stream filled the
	// per-frame metadata budget. No prompts or tool arguments go in this file.
	if stage == "terminal" && len(raw) <= 16<<10 {
		_ = os.WriteFile(filepath.Join(d.dir, "terminal.json"), raw, 0600)
	}
	if d.metadataBytes+len(raw) > 256<<10 {
		return
	}
	f, err := os.OpenFile(filepath.Join(d.dir, "metadata.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	n, _ := f.Write(raw)
	d.metadataBytes += n
}

func (d *traeCNDiagnostic) artifact(name string, body []byte, total int) {
	if d == nil || d.dir == "" || !d.raw {
		return
	}
	size := min(len(body), d.limit)
	safe := d.redact(body[:size])
	// Redaction can grow small values; the disk limit still applies.
	truncated := total > size || len(safe) > d.limit
	safe = safe[:min(len(safe), d.limit)]
	if err := os.WriteFile(filepath.Join(d.dir, name), safe, 0600); err != nil {
		d.record("capture_error", map[string]any{"file": name})
		return
	}
	d.record("capture", map[string]any{"file": name, "observed_bytes": total, "captured_bytes": len(safe), "truncated": truncated})
	if truncated {
		// An independent marker survives a full metadata log.
		_ = os.WriteFile(filepath.Join(d.dir, name+".truncated"), []byte(strconv.Itoa(total)), 0600)
	}
}

type traeCNDiagnosticReader struct {
	io.ReadCloser
	d     *traeCNDiagnostic
	name  string
	buf   bytes.Buffer
	total int
	once  sync.Once
	mu    sync.Mutex
}

func (r *traeCNDiagnosticReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	r.total += n
	if remaining := r.d.limit - r.buf.Len(); remaining > 0 {
		r.buf.Write(p[:min(n, remaining)])
	}
	r.mu.Unlock()
	if err != nil {
		r.save()
	}
	return n, err
}
func (r *traeCNDiagnosticReader) save() {
	r.once.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.d.artifact(r.name, r.buf.Bytes(), r.total)
	})
}
func (r *traeCNDiagnosticReader) Close() error {
	err := r.ReadCloser.Close()
	r.save()
	return err
}
func (d *traeCNDiagnostic) reader(source io.ReadCloser, name string) io.ReadCloser {
	if d == nil || d.dir == "" || !d.raw {
		return source
	}
	return &traeCNDiagnosticReader{ReadCloser: source, d: d, name: name}
}
func (d *traeCNDiagnostic) upstream(source io.ReadCloser) io.ReadCloser {
	if d == nil || d.dir == "" {
		return source
	}
	d.mu.Lock()
	name := fmt.Sprintf("upstream-%d.sse", d.upstreamCount)
	d.upstreamCount++
	d.mu.Unlock()
	return d.reader(source, name)
}

func traeCNDiagnosticRequestSummary(body []byte) map[string]any {
	root := gjson.ParseBytes(body)
	result := map[string]any{"bytes": len(body)}
	// Full values of stop and tool_choice are in raw artifacts only: stop strings
	// and arbitrary choice objects can contain user content.
	for _, field := range []string{"model", "config_name", "function", "reasoning.effort", "reasoning_effort", "max_output_tokens", "max_completion_tokens", "max_tokens", "parallel_tool_calls", "access_type", "tool_mode"} {
		if v := root.Get(field); v.Exists() {
			result[field] = v.Value()
		}
	}
	choice := root.Get("tool_choice")
	if choice.IsObject() {
		result["tool_choice"] = map[string]any{"type": choice.Get("type").String(), "name": traeCNDeclarationName(choice), "namespace": traeFirstText(choice, "namespace", "function.namespace")}
	} else if choice.Exists() {
		result["tool_choice"] = choice.Value()
	}
	result["stop_present"] = root.Get("stop").Exists()
	result["stop_type"] = root.Get("stop").Type.String()
	var tools []map[string]any
	var walk func(gjson.Result, string, string)
	walk = func(list gjson.Result, namespace, source string) {
		for _, tool := range list.Array() {
			typ, name := tool.Get("type").String(), traeCNDeclarationName(tool)
			if typ == "namespace" {
				child := name
				if namespace != "" {
					child = namespace + "." + name
				}
				walk(traeCNNamespaceChildren(tool), child, source)
				continue
			}
			disposition := "function"
			switch typ {
			case "", "function":
			case "custom", "apply_patch", "local_shell", "shell", "tool_search":
				disposition = "bridge"
			default:
				disposition = "unsupported"
			}
			tools = append(tools, map[string]any{"name": name, "type": typ, "namespace": namespace, "source": source, "conversion": disposition})
		}
	}
	walk(root.Get("tools"), "", "tools")
	inputTypes := map[string]int{}
	for _, item := range root.Get("input").Array() {
		typ := item.Get("type").String()
		inputTypes[typ]++
		if typ == "additional_tools" || typ == "tool_search_output" || typ == "tool_search_call_output" {
			walk(item.Get("tools"), "", typ)
		}
	}
	result["tools"], result["tool_count"], result["input_types"] = tools, len(tools), inputTypes
	result["messages"] = len(root.Get("messages").Array())
	return result
}

func (d *traeCNDiagnostic) recordParsed(event string, parsed gjson.Result, payloadEvent string) {
	if d == nil || d.dir == "" {
		return
	}
	toolEvent := payloadEvent
	if toolEvent == "" {
		toolEvent = event
	}
	paths := map[string]int{}
	for _, path := range []string{"tool_calls", "message.tool_calls", "choices.0.delta.tool_calls", "choices.0.message.tool_calls"} {
		v := parsed.Get(path)
		if v.IsArray() {
			paths[path] = len(v.Array())
		} else if v.IsObject() {
			paths[path] = 1
		}
	}
	reason, path := "", ""
	for _, p := range []string{"finish_reason", "stop_reason", "choices.0.finish_reason", "reason"} {
		if value := traeFirstText(parsed, p); value != "" {
			reason, path = value, p
			break
		}
	}
	d.record("parsed", map[string]any{"event": event, "payload_event": payloadEvent, "tool_fields": paths, "parsed_tool_calls": len(traeToolCalls(parsed, toolEvent)), "raw_finish_reason": responsesIdentityLogValue(reason), "finish_reason_path": path})
	if event == "metadata" || event == "timing_cost" || event == "extra_info" {
		for _, field := range []string{"model", "provider_model_name"} {
			if value := parsed.Get(field); value.Type == gjson.String && value.String() != "" {
				d.record("provider_model", map[string]any{"event": event, "field": field, "value": responsesIdentityLogValue(value.String()), "is_retry": parsed.Get("is_retry").Value()})
			}
		}
	}
}
