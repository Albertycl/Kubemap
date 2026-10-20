package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.json from examples")

func TestEveryTypeHasExample(t *testing.T) {
	seen := map[Type]bool{}
	for _, ex := range examples() {
		seen[ex.body.MessageType()] = true
	}
	for _, typ := range Types {
		if !seen[typ] {
			t.Errorf("no example for %s", typ)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, ex := range examples() {
		t.Run(ex.name, func(t *testing.T) {
			data, err := Encode(ex.id, ex.body)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			env, body, err := Decode(data)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if env.V != Version || env.Type != ex.body.MessageType() || env.ID != ex.id {
				t.Errorf("envelope = {%d %s %q}, want {%d %s %q}", env.V, env.Type, env.ID, Version, ex.body.MessageType(), ex.id)
			}
			if !reflect.DeepEqual(body, ex.body) {
				t.Errorf("body mismatch\n got: %#v\nwant: %#v", body, ex.body)
			}
		})
	}
}

func TestGolden(t *testing.T) {
	for _, ex := range examples() {
		t.Run(ex.name, func(t *testing.T) {
			path := filepath.Join("testdata", ex.name+".json")
			data, err := Encode(ex.id, ex.body)
			if err != nil {
				t.Fatal(err)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, data, "", "  "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteByte('\n')
			if *update {
				if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden file (run go test -update): %v", err)
			}
			if !bytes.Equal(golden, pretty.Bytes()) {
				t.Errorf("%s is out of date (run go test -update)\n got: %s\nwant: %s", path, pretty.Bytes(), golden)
			}
			// The golden file must also decode on its own, as a peer would read it.
			if _, _, err := Decode(golden); err != nil {
				t.Errorf("golden file does not decode: %v", err)
			}
		})
	}
}

// Golden files are documentation; they must hold placeholders only.
func TestGoldenHasNoSecrets(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden files: %v", err)
	}
	jwt := regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}`)
	secretKeys := []string{"importToken", "credential", "serviceAccountToken", "token"}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if jwt.Match(data) {
			t.Errorf("%s contains something that looks like a JWT", f)
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		walk(v, func(key string, val any) {
			s, ok := val.(string)
			if !ok {
				return
			}
			for _, k := range secretKeys {
				if key == k && !strings.HasSuffix(s, "_example") {
					t.Errorf("%s: %s = %q is not a placeholder ending in _example", f, key, s)
				}
			}
		})
	}
}

func walk(v any, fn func(key string, val any)) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			fn(k, val)
			walk(val, fn)
		}
	case []any:
		for _, val := range x {
			walk(val, fn)
		}
	}
}

func code(err error) ErrorCode {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestValidateRejects(t *testing.T) {
	big := make([]byte, MaxMessageSize+1)
	for i := range big {
		big[i] = ' '
	}
	tests := []struct {
		name string
		env  *Envelope
		want ErrorCode
	}{
		{"nil", nil, CodeBadRequest},
		{"missing type", &Envelope{V: 1, ID: "a", Payload: json.RawMessage(`{}`)}, CodeBadRequest},
		{"wrong version", &Envelope{V: 2, Type: TypeHeartbeat, Payload: json.RawMessage(`{}`)}, CodeUnsupported},
		{"version zero", &Envelope{Type: TypeHeartbeat, Payload: json.RawMessage(`{}`)}, CodeUnsupported},
		{"unknown type", &Envelope{V: 1, Type: "exec", ID: "a"}, CodeUnsupported},
		{"request without id", &Envelope{V: 1, Type: TypeRequest, Payload: json.RawMessage(`{}`)}, CodeBadRequest},
		{"response without id", &Envelope{V: 1, Type: TypeResponse, Payload: json.RawMessage(`{}`)}, CodeBadRequest},
		{"stream.data without id", &Envelope{V: 1, Type: TypeStreamData, Payload: json.RawMessage(`{}`)}, CodeBadRequest},
		{"oversized payload", &Envelope{V: 1, Type: TypeStreamData, ID: "s", Payload: big}, CodeTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := code(Validate(tt.env)); got != tt.want {
				t.Errorf("Validate code = %q, want %q", got, tt.want)
			}
		})
	}
	if err := Validate(&Envelope{V: 1, Type: TypeHeartbeat, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Errorf("heartbeat without id should pass: %v", err)
	}
}

func TestDecodeUnknownTypeKeepsID(t *testing.T) {
	env, body, err := Decode([]byte(`{"v":1,"type":"exec","id":"x-9","payload":{"cmd":"sh"}}`))
	if code(err) != CodeUnsupported {
		t.Fatalf("err = %v, want code unsupported", err)
	}
	if env == nil || env.ID != "x-9" || body != nil {
		t.Fatalf("env = %+v, body = %v; want id x-9 and no body", env, body)
	}
	// The caller answers with an error message carrying the same id.
	reply, err := Encode(env.ID, AsError(err))
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := Decode(reply)
	if err != nil || got.(*Error).Code != CodeUnsupported {
		t.Fatalf("reply = %s, err = %v", reply, err)
	}
}

func TestDecodeRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want ErrorCode
	}{
		{"not json", `hello`, CodeBadRequest},
		{"array", `[1,2]`, CodeBadRequest},
		{"no payload", `{"v":1,"type":"heartbeat"}`, CodeBadRequest},
		{"null payload", `{"v":1,"type":"heartbeat","payload":null}`, CodeBadRequest},
		{"payload wrong shape", `{"v":1,"type":"heartbeat","payload":{"time":42}}`, CodeBadRequest},
		{"invalid body", `{"v":1,"type":"heartbeat","payload":{}}`, CodeBadRequest},
		{"unknown op", `{"v":1,"type":"request","id":"q","payload":{"op":"delete"}}`, CodeUnsupported},
		{"missing id", `{"v":1,"type":"request","payload":{"op":"list","version":"v1","resource":"pods"}}`, CodeBadRequest},
		{"wrong version", `{"v":2,"type":"heartbeat","payload":{"time":"2026-09-27T12:00:00Z"}}`, CodeUnsupported},
		{"too large", `{"v":1,"type":"heartbeat","payload":"` + strings.Repeat("a", MaxMessageSize) + `"}`, CodeTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := Decode([]byte(tt.in)); code(err) != tt.want {
				t.Errorf("Decode err = %v, want code %q", err, tt.want)
			}
		})
	}
}

func TestDecodeIgnoresUnknownFields(t *testing.T) {
	_, body, err := Decode([]byte(`{"v":1,"type":"heartbeat","extra":1,"payload":{"time":"2026-09-27T12:00:00Z","seq":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !body.(*Heartbeat).Time.Equal(exampleTime) {
		t.Errorf("time = %v", body.(*Heartbeat).Time)
	}
}

// Decode must never panic, whatever arrives on the wire.
func FuzzDecode(f *testing.F) {
	for _, ex := range examples() {
		data, _ := Encode(ex.id, ex.body)
		f.Add(data)
	}
	f.Add([]byte(`{"v":1,"type":"exec","id":"a"}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		env, body, err := Decode(data)
		if err == nil && (env == nil || body == nil) {
			t.Fatal("nil result without error")
		}
	})
}

func TestEncodeRejects(t *testing.T) {
	tests := []struct {
		name string
		id   string
		body Payload
		want ErrorCode
	}{
		{"nil body", "a", nil, CodeBadRequest},
		{"invalid body", "", &Heartbeat{}, CodeBadRequest},
		{"request without id", "", &Request{Op: OpList, Version: "v1", Resource: "pods"}, CodeBadRequest},
		{"chunk too large", "s", &StreamData{Data: make([]byte, MaxStreamChunk+1)}, CodeTooLarge},
		{"logs too large", "q", &Response{Logs: make([]byte, MaxLogBytes+1)}, CodeTooLarge},
		{"message too large", "q", &Response{Object: json.RawMessage(`"` + strings.Repeat("a", MaxMessageSize) + `"`)}, CodeTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Encode(tt.id, tt.body); code(err) != tt.want {
				t.Errorf("Encode err = %v, want code %q", err, tt.want)
			}
		})
	}
}

// B2: the largest chunk fits in one message and round-trips byte for byte,
// whatever it holds. JSON strings would escape each of these bytes to 6.
func TestMaxChunkWorstCase(t *testing.T) {
	invalid := make([]byte, MaxStreamChunk)
	for i := range invalid {
		invalid[i] = byte(0x80 + i%64) // invalid UTF-8
	}
	cases := map[string][]byte{
		"ansi escape":  bytes.Repeat([]byte{0x1b}, MaxStreamChunk),
		"html":         bytes.Repeat([]byte("<"), MaxStreamChunk),
		"nul":          make([]byte, MaxStreamChunk),
		"invalid utf8": invalid,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			msg, err := Encode("s-1", &StreamData{Seq: 1 << 40, Data: data})
			if err != nil {
				t.Fatalf("stream.data: %v", err)
			}
			_, body, err := Decode(msg)
			if err != nil || !bytes.Equal(body.(*StreamData).Data, data) {
				t.Fatalf("stream.data round trip changed the bytes (err %v)", err)
			}
			msg, err = Encode("q-1", &Response{Logs: data, Truncated: true})
			if err != nil {
				t.Fatalf("logs response: %v", err)
			}
			if _, body, err = Decode(msg); err != nil || !bytes.Equal(body.(*Response).Logs, data) {
				t.Fatalf("logs round trip changed the bytes (err %v)", err)
			}
		})
	}
	_, body, _ := Decode(mustEncode(t, "s", &StreamData{Data: []byte("a\xffb")}))
	if string(body.(*StreamData).Data) != "a\xffb" {
		t.Errorf("invalid UTF-8 was rewritten: %q", body.(*StreamData).Data)
	}
}

func mustEncode(t *testing.T, id string, p Payload) []byte {
	t.Helper()
	b, err := Encode(id, p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// B1: the portal's rule for node registration.
func TestAcceptNode(t *testing.T) {
	good := TokenReviewResult{Authenticated: true, Audiences: []string{"other", NodeTokenAudience},
		Username: NodeAgentUsername, NodeName: "k8server"}
	if err := good.AcceptNode("k8server"); err != nil {
		t.Errorf("good token rejected: %v", err)
	}
	bad := map[string]func(r *TokenReviewResult){
		"not authenticated": func(r *TokenReviewResult) { r.Authenticated = false },
		"no audiences":      func(r *TokenReviewResult) { r.Audiences = nil },
		"other audience":    func(r *TokenReviewResult) { r.Audiences = []string{"https://kubernetes.default.svc"} },
		"default SA":        func(r *TokenReviewResult) { r.Username = "system:serviceaccount:default:default" },
		"other node":        func(r *TokenReviewResult) { r.NodeName = "node-2" },
		"no node claim":     func(r *TokenReviewResult) { r.NodeName = "" },
	}
	for name, mutate := range bad {
		r := good
		r.Audiences = slices.Clone(good.Audiences)
		mutate(&r)
		if err := r.AcceptNode("k8server"); code(err) != CodeUnauthorized {
			t.Errorf("%s: err = %v, want unauthorized", name, err)
		}
	}
	var none *TokenReviewResult
	if code(none.AcceptNode("k8server")) != CodeUnauthorized {
		t.Error("nil result must be unauthorized")
	}
	if code(good.AcceptNode("")) != CodeUnauthorized {
		t.Error("empty register nodeName must be unauthorized")
	}
}

func TestPayloadValidate(t *testing.T) {
	problems := make([]Problem, MaxProblems+1)
	for i := range problems {
		problems[i] = Problem{Kind: "Pod", Name: "p", Reason: "r"}
	}
	bad := []Payload{
		&Register{Role: RoleNode, AgentVersion: "v", ClusterID: "c", NodeName: "n"},
		&Register{Role: "admin", AgentVersion: "v"},
		&Register{Role: "cluster", AgentVersion: "v", ClusterID: "c", NodeName: "n", ServiceAccountToken: "t"},
		&RegisterAck{Status: StatusActive},
		&RegisterAck{ClusterID: "c", Status: "rejected"},
		&Summary{},
		&Summary{Time: exampleTime, Problems: problems},
		&Summary{Time: exampleTime, Problems: []Problem{{Kind: "Pod"}}},
		&Metrics{Time: exampleTime},
		&Metrics{Time: exampleTime, NodeName: "n", TopProcesses: make([]ProcessStats, MaxTopProcesses+1)},
		&Request{Op: OpList},
		&Request{Op: OpGet, Version: "v1", Resource: "pods"},
		&Request{Op: OpLogs, Name: "p"},
		&Request{Op: OpTokenReview},
		&Request{Op: OpTokenReview, TokenReview: &TokenReviewRequest{Token: "t"}},
		&Request{Op: OpTokenReview, TokenReview: &TokenReviewRequest{Token: "t", Audience: "https://kubernetes.default.svc"}},
		&Request{Op: OpList, Version: "v1", Resource: "pods", Limit: -5},
		&Request{Op: OpList, Version: "v1", Resource: "pods", Limit: MaxListLimit + 1},
		&Request{Op: OpLogs, Namespace: "n", Name: "p", Logs: &LogOptions{TailLines: -1}},
		&Request{Op: OpLogs, Namespace: "n", Name: "p", Logs: &LogOptions{SinceSeconds: -1}},
		&Request{Op: OpGet, Version: "v1", Resource: "secrets", Namespace: "n", Name: "s"},
		&Request{Op: OpList, Version: "v1", Resource: "secrets"},
		&Request{Op: OpGet, Version: "v1", Resource: "pods", Namespace: "../kube-system", Name: "p"},
		&Request{Op: OpGet, Version: "v1", Resource: "pods/exec", Namespace: "n", Name: "p"},
		&Request{Op: OpGet, Version: "v1", Resource: "pods", Namespace: "n", Name: ".."},
		&Request{Op: OpList, Group: "apps/v1", Version: "v1", Resource: "deployments"},
		&Request{Op: OpGraph, Graph: &GraphOptions{Depth: MaxGraphDepth + 1}},
		&Request{Op: OpGraph, Graph: &GraphOptions{Depth: -1}},
		&Request{Op: OpChain},
		&Request{Op: OpChain, Chain: &ChainRequest{}},
		&Request{Op: OpChain, Chain: &ChainRequest{ID: "Pod/a/b", Query: "b"}},
		&Request{Op: OpChain, Chain: &ChainRequest{ID: "Pod/a"}},
		&Request{Op: OpChain, Chain: &ChainRequest{ID: "Pod/a/b c"}},
		&Request{Op: OpChain, Chain: &ChainRequest{Query: "two\nlines"}},
		&Response{Error: &Error{Code: "oops"}},
		&Response{Error: &Error{Code: CodeNotFound}, Object: json.RawMessage(`{}`)},
		&Response{Object: json.RawMessage(`{}`), Logs: []byte("x")},
		&Response{Logs: make([]byte, MaxLogBytes+1)},
		&Summary{Time: exampleTime, NodeList: make([]NodeStatus, MaxSummaryNodes+1)},
		&StreamOpen{Kind: "exec", Namespace: "n", Name: "p"},
		&StreamOpen{Kind: StreamLogs, Namespace: "n"},
		&StreamClose{Error: &Error{Code: "oops"}},
		&Error{},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("case %d (%T %+v): Validate passed, want error", i, p, p)
		}
	}
	if err := (&Response{}).Validate(); err != nil {
		t.Errorf("empty list response: %v", err)
	}
	if err := (&Request{Op: OpList, Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}).Validate(); err != nil {
		t.Errorf("list clusterroles: %v", err)
	}
	if err := (&Request{Op: OpGet, Version: "v1", Resource: "clusterroles", Name: "system:controller:x", Group: "rbac.authorization.k8s.io"}).Validate(); err != nil {
		t.Errorf("colon in name: %v", err)
	}
	if err := (&StreamClose{Error: &Error{Code: CodeForbidden, Message: "no"}}).Validate(); err != nil {
		t.Errorf("stream.close with error: %v", err)
	}
}

func TestAsError(t *testing.T) {
	if AsError(nil) != nil {
		t.Error("AsError(nil) != nil")
	}
	if got := AsError(errors.New("boom")); got.Code != CodeInternal || got.Message != "boom" {
		t.Errorf("plain error -> %+v", got)
	}
	wrapped := fmt.Errorf("ctx: %w", &Error{Code: CodeForbidden, Message: "rbac"})
	if got := AsError(wrapped); got.Code != CodeForbidden {
		t.Errorf("wrapped -> %+v", got)
	}
	if s := (&Error{Code: CodeNotFound, Message: "x"}).Error(); s != "protocol: not_found: x" {
		t.Errorf("Error() = %q", s)
	}
}

func TestLogValueRedacts(t *testing.T) {
	regMsg := mustEncode(t, "r", &Register{Role: RoleNode, AgentVersion: "v", ClusterID: "c", NodeName: "n", ServiceAccountToken: "secret-env"})
	env, _, err := Decode(regMsg)
	if err != nil {
		t.Fatal(err)
	}
	req := &Request{Op: OpTokenReview, TokenReview: &TokenReviewRequest{Token: "secret-req", Audience: NodeTokenAudience}}
	for name, h := range map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		log := slog.New(h(&buf))
		log.Info("x",
			"reg", Register{Role: RoleNode, AgentVersion: "v", ClusterID: "c", ServiceAccountToken: "secret-sa"},
			"ack", RegisterAck{ClusterID: "c", Status: StatusActive},
			"tr", TokenReviewRequest{Token: "secret-tr", Audience: NodeTokenAudience},
			"req", req, "reqValue", *req,
			"env", env, "envValue", *env,
		)
		out := buf.String()
		if strings.Contains(out, "secret") {
			t.Errorf("%s log leaks a secret: %s", name, out)
		}
		for _, want := range []string{"[redacted]", "payloadBytes", "tokenreview"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s log lacks %q: %s", name, want, out)
			}
		}
	}
}

func TestKnownAndNeedsID(t *testing.T) {
	if Known("exec") || !Known(TypeError) {
		t.Error("Known is wrong")
	}
	if NeedsID(TypeHeartbeat) || NeedsID(TypeError) || !NeedsID(TypeRequest) || !NeedsID(TypeResponse) {
		t.Error("NeedsID is wrong")
	}
}

// TestAppMapGolden checks that the appmap response example decodes into
// AppMap with no unknown fields, as the portal will read it, and uses only
// known tiers and evidence types.
func TestAppMapGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "response-appmap.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, p, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(p.(*Response).Object))
	dec.DisallowUnknownFields()
	var m AppMap
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if len(m.Apps) != 2 || len(m.Links) != 4 || len(m.Peers) != 1 {
		t.Fatalf("appmap: %d apps, %d links, %d peers", len(m.Apps), len(m.Links), len(m.Peers))
	}
	evidence := map[EvidenceType]bool{EvidenceRoute: true, EvidenceEnv: true, EvidenceConfigMap: true, EvidenceFile: true, EvidenceOperator: true}
	for _, a := range m.Apps {
		for _, c := range append(a.Components, m.Peers...) {
			if !slices.Contains(Tiers, c.Tier) || c.TierReason == "" {
				t.Errorf("%s: tier %q (%q)", c.ID, c.Tier, c.TierReason)
			}
		}
	}
	for _, l := range m.Links {
		for _, e := range l.Evidence {
			if !evidence[e.Type] || e.Object == "" || e.Detail == "" {
				t.Errorf("%s → %s: evidence %+v", l.From, l.To, e)
			}
		}
	}
}

func TestRequestAppMapValidate(t *testing.T) {
	for _, tc := range []struct {
		opts *AppMapOptions
		ok   bool
	}{
		{nil, true},
		{&AppMapOptions{}, true},
		{&AppMapOptions{App: "argocd/flask-vue"}, true},
		{&AppMapOptions{App: "a b"}, false},
		{&AppMapOptions{App: strings.Repeat("x", 513)}, false},
	} {
		err := (&Request{Op: OpAppMap, AppMap: tc.opts}).Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%+v: err = %v", tc.opts, err)
		}
	}
}

func TestRequestNetpolCheckValidate(t *testing.T) {
	pod := NetpolEndpoint{Namespace: "shop", Pod: "web"}
	for _, tc := range []struct {
		name string
		r    *NetpolCheckRequest
		ok   bool
	}{
		{"pod to pod", &NetpolCheckRequest{From: pod, To: NetpolEndpoint{Namespace: "shop", Pod: "db"}, Port: 5432}, true},
		{"ip to service, UDP", &NetpolCheckRequest{From: NetpolEndpoint{IP: "203.0.113.7"}, To: NetpolEndpoint{Namespace: "kube-system", Service: "kube-dns"}, Port: 53, Protocol: "UDP"}, true},
		{"missing", nil, false},
		{"port 0", &NetpolCheckRequest{From: pod, To: pod, Port: 0}, false},
		{"port too big", &NetpolCheckRequest{From: pod, To: pod, Port: 70000}, false},
		{"bad protocol", &NetpolCheckRequest{From: pod, To: pod, Port: 80, Protocol: "tcp"}, false},
		{"service as source", &NetpolCheckRequest{From: NetpolEndpoint{Namespace: "a", Service: "b"}, To: pod, Port: 80}, false},
		{"two kinds in one end", &NetpolCheckRequest{From: pod, To: NetpolEndpoint{Namespace: "a", Pod: "b", IP: "10.0.0.1"}, Port: 80}, false},
		{"empty end", &NetpolCheckRequest{From: pod, To: NetpolEndpoint{}, Port: 80}, false},
		{"pod without namespace", &NetpolCheckRequest{From: pod, To: NetpolEndpoint{Pod: "b"}, Port: 80}, false},
		{"slash in name", &NetpolCheckRequest{From: pod, To: NetpolEndpoint{Namespace: "a", Pod: "b/c"}, Port: 80}, false},
		{"bad ip", &NetpolCheckRequest{From: NetpolEndpoint{IP: "1.2.3"}, To: pod, Port: 80}, false},
	} {
		err := (&Request{Op: OpNetpolCheck, NetpolCheck: tc.r}).Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}

func TestRequestPodDiagnoseValidate(t *testing.T) {
	if err := (&Request{Op: OpPodDiagnose, Namespace: "a", Name: "b"}).Validate(); err != nil {
		t.Errorf("valid: %v", err)
	}
	for _, r := range []*Request{{Op: OpPodDiagnose, Name: "b"}, {Op: OpPodDiagnose, Namespace: "a"}, {Op: OpPodDiagnose, Namespace: "a", Name: "b/c"}} {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

func TestValidateNodePressure(t *testing.T) {
	for _, r := range []*Request{{Op: OpNodePressure}, {Op: OpNodePressure, Name: "k8server"},
		{Op: OpNodePressure, NodeMetrics: []Metrics{{NodeName: "a"}, {NodeName: "b"}}}} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	for _, r := range []*Request{{Op: OpNodePressure, Namespace: "x"}, {Op: OpNodePressure, Name: "a/b"},
		{Op: OpNodePressure, NodeMetrics: []Metrics{{NodeName: ""}}},
		{Op: OpNodePressure, NodeMetrics: []Metrics{{NodeName: "a"}, {NodeName: "a"}}},
		{Op: OpNodePressure, NodeMetrics: []Metrics{{NodeName: "a/b"}}},
		{Op: OpNodePressure, NodeMetrics: make([]Metrics, MaxNodeReports+1)},
		{Op: OpAppMap, NodeMetrics: []Metrics{{NodeName: "a"}}}} {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v must fail", r)
		}
	}
}

func TestRequestServiceCheckValidate(t *testing.T) {
	for _, r := range []*Request{{Op: OpServiceCheck}, {Op: OpServiceCheck, Namespace: "a"}, {Op: OpServiceCheck, Namespace: "a", Name: "b"}} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if err := (&Request{Op: OpServiceCheck, Name: "b"}).Validate(); err == nil {
		t.Error("a name without a namespace was accepted")
	}
}

func TestRequestPodDebugValidate(t *testing.T) {
	if err := (&Request{Op: OpPodDebug, Namespace: "a", Name: "b"}).Validate(); err != nil {
		t.Errorf("valid: %v", err)
	}
	for _, r := range []*Request{{Op: OpPodDebug, Name: "b"}, {Op: OpPodDebug, Namespace: "a"}} {
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "pod.debug") {
			t.Errorf("%+v: %v", r, err)
		}
	}
}

func TestValidateHealthCheck(t *testing.T) {
	for _, r := range []*Request{{Op: OpHealthCheck}, {Op: OpHealthCheck, Namespace: "kubemap-demo"}} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if err := (&Request{Op: OpHealthCheck, Namespace: "a/b"}).Validate(); err == nil {
		t.Error("bad namespace must fail")
	}
	if err := (&Request{Op: OpHealthCheck, NodeMetrics: []Metrics{{NodeName: "a"}}}).Validate(); err != nil {
		t.Errorf("node reports: %v", err)
	}
	if err := (&Request{Op: OpHealthCheck, NodeMetrics: []Metrics{{NodeName: "a"}, {NodeName: "a"}}}).Validate(); err == nil {
		t.Error("duplicate node reports must fail")
	}
}

func TestValidatePodUsage(t *testing.T) {
	for _, r := range []*Request{{Op: OpPodUsage}, {Op: OpPodUsage, Namespace: "a"}, {Op: OpPodUsage, Namespace: "a", Name: "b"}} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if err := (&Request{Op: OpPodUsage, Name: "b"}).Validate(); err == nil {
		t.Error("a name without a namespace must fail")
	}
}

// Kubemap is read-only by design: there is no op that changes the cluster.
func TestNoWriteOps(t *testing.T) {
	for _, op := range []Op{"action", "scale", "delete", "patch", "cordon"} {
		if err := (&Request{Op: op}).Validate(); err == nil {
			t.Errorf("op %q must be refused", op)
		}
	}
}

func TestValidateRBACWhoCan(t *testing.T) {
	sa := &RBACSubject{Kind: SubjectServiceAccount, Namespace: "kubemap-demo", Name: "reporter"}
	for _, q := range []*RBACQuery{
		{Subject: sa},
		{Subject: sa, Verb: "list", Resource: "pods", Namespace: "sre-lab-app"},
		{Verb: "get", Resource: "secrets", Namespace: "sre-lab-app"},
		{Verb: "patch", Resource: "deployments/scale", APIGroup: "apps"},
		{Subject: &RBACSubject{Kind: SubjectUser, Name: "alice@example.com", Groups: []string{"ops"}}},
		{Subject: &RBACSubject{Kind: SubjectGroup, Name: "system:authenticated"}},
	} {
		if err := (&Request{Op: OpRBACWhoCan, RBAC: q}).Validate(); err != nil {
			t.Errorf("%+v: %v", q, err)
		}
	}
	for name, q := range map[string]*RBACQuery{
		"missing":       nil,
		"empty":         {},
		"verb only":     {Verb: "get"},
		"resource only": {Resource: "pods"},
		"bad kind":      {Subject: &RBACSubject{Kind: "Robot", Name: "x"}},
		"no name":       {Subject: &RBACSubject{Kind: SubjectUser}},
		"sa without ns": {Subject: &RBACSubject{Kind: SubjectServiceAccount, Name: "x"}},
		"two slashes":   {Verb: "get", Resource: "a/b/c"},
		"space in verb": {Verb: "get all", Resource: "pods"},
		"bad namespace": {Verb: "get", Resource: "pods", Namespace: "a/b"},
	} {
		var err error
		if q == nil {
			err = (&Request{Op: OpRBACWhoCan}).Validate()
		} else {
			err = (&Request{Op: OpRBACWhoCan, RBAC: q}).Validate()
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if (&RBACQuery{Subject: sa}).Mode() != RBACChain || (&RBACQuery{Subject: sa, Verb: "get", Resource: "pods"}).Mode() != RBACCheck ||
		(&RBACQuery{Verb: "get", Resource: "pods"}).Mode() != RBACWhoCan {
		t.Error("modes")
	}
}

// review-km-44 M1: group, version and resource end up in API paths; only
// Kubernetes' own shapes pass, and a Secret is refused in any group.
func TestValidateRejectsPathTricks(t *testing.T) {
	get := func(group, version, resource, ns, name string) *Request {
		return &Request{Op: OpGet, Group: group, Version: version, Resource: resource, Namespace: ns, Name: name}
	}
	for name, r := range map[string]*Request{
		"the review's payload": get("x", "../../api/v1", "secrets", "kubemap-system", "kubemap-agent"),
		"version with a path":  get("apps", "v1/../../api/v1", "deployments", "a", "b"),
		"version dots":         get("apps", "..", "deployments", "a", "b"),
		"version upper case":   get("apps", "V1", "deployments", "a", "b"),
		"version other form":   get("apps", "v1.2", "deployments", "a", "b"),
		"group with a path":    get("x/../..", "v1", "configmaps", "a", "b"),
		"group dots":           get("..", "v1", "configmaps", "a", "b"),
		"group upper case":     get("Apps", "v1", "deployments", "a", "b"),
		"group with a space":   get("a pps", "v1", "deployments", "a", "b"),
		"resource dots":        get("", "v1", "..", "a", "b"),
		"resource upper case":  get("", "v1", "ConfigMaps", "a", "b"),
		"resource with a dot":  get("", "v1", "configmaps.x", "a", "b"),
	} {
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "bad_request") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, r := range map[string]*Request{
		"core secrets":        get("", "v1", "secrets", "a", "b"),
		"secrets, any group":  get("x", "v1", "secrets", "a", "b"),
		"secrets, core group": get("core", "v1", "secrets", "a", "b"),
	} {
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, r := range []*Request{
		get("", "v1", "configmaps", "a", "b"),
		get("apps", "v1", "deployments", "a", "b"),
		get("metrics.k8s.io", "v1beta1", "pods", "a", "b"),
		get("autoscaling", "v2", "horizontalpodautoscalers", "a", "b"),
		get("argoproj.io", "v1alpha1", "applications", "argocd", "flask-vue"),
		get("cert-manager.io", "v1", "certificates", "a", "b"),
		{Op: OpList, Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
	} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
}
