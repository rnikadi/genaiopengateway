package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rnikadi/genaiopengateway/internal/a2apb"
)

// grpcAgent is a fake agent that speaks the gRPC binding of A2A 1.0.
type grpcAgent struct {
	addr string
	mu   sync.Mutex
	seen []string // "Method x-internal | request as JSON"
}

func newGRPCAgent(t *testing.T) *grpcAgent {
	t.Helper()
	svc, err := a2apb.Service()
	if err != nil {
		t.Fatal(err)
	}
	a := &grpcAgent{}
	task := func(id, text string) string {
		return `{"id":"` + id + `","contextId":"ctx-` + id + `","status":{"state":"TASK_STATE_COMPLETED"},"artifacts":[{"artifactId":"a","parts":[{"text":"echo: ` + text + `"}]}]}`
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		full, _ := grpc.MethodFromServerStream(stream)
		name := full[strings.LastIndexByte(full, '/')+1:]
		md := svc.Methods().ByName(protoreflect.Name(name))
		in := dynamicpb.NewMessage(md.Input())
		if err := stream.RecvMsg(in); err != nil {
			return err
		}
		raw, _ := protojson.Marshal(in)
		meta, _ := metadata.FromIncomingContext(stream.Context())
		a.mu.Lock()
		a.seen = append(a.seen, name+" "+strings.Join(meta.Get("x-internal"), "")+strings.Join(meta.Get("authorization"), "")+" | "+string(raw))
		a.mu.Unlock()
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		reply := func(js string) error {
			out := dynamicpb.NewMessage(md.Output())
			if err := protojson.Unmarshal([]byte(js), out); err != nil {
				return err
			}
			return stream.SendMsg(out)
		}
		switch name {
		case "SendMessage":
			text := ""
			if msg, _ := req["message"].(map[string]any); msg != nil {
				if parts, _ := msg["parts"].([]any); len(parts) > 0 {
					text, _ = parts[0].(map[string]any)["text"].(string)
				}
			}
			return reply(`{"task":` + task("grpc-task", text) + `}`)
		case "SendStreamingMessage", "SubscribeToTask":
			if err := reply(`{"task":{"id":"stream-task","contextId":"stream-ctx","status":{"state":"TASK_STATE_WORKING"}}}`); err != nil {
				return err
			}
			return reply(`{"statusUpdate":{"taskId":"stream-task","contextId":"stream-ctx","status":{"state":"TASK_STATE_COMPLETED"}}}`)
		case "GetTask":
			id, _ := req["id"].(string)
			return reply(task(id, "got"))
		case "CancelTask":
			st, _ := status.New(codes.FailedPrecondition, "done already").WithDetails(&errdetails.ErrorInfo{Reason: "TASK_NOT_CANCELABLE", Domain: "a2a-protocol.org"})
			return st.Err()
		}
		return status.Error(codes.Unimplemented, name)
	}))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	a.addr = lis.Addr().String()
	return a
}

func (a *grpcAgent) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.seen) == 0 {
		return ""
	}
	return a.seen[len(a.seen)-1]
}

// The gRPC binding of A2A 1.0 gets the checks the other bindings get.
func TestA2AGRPC(t *testing.T) {
	agent := newGRPCAgent(t)
	card := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"Planner","version":"1","supportedInterfaces":[{"url":"internal:50051","protocolBinding":"GRPC","protocolVersion":"1.0","tenant":"inner"}],"capabilities":{"streaming":true},"skills":[]}`)
	}))
	t.Cleanup(card.Close)
	e := newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
a2a:
  public_url: https://gw.example.com
  agents:
    planner: {grpc_url: "grpc://`+agent.addr+`", grpc_tenant: inner, card_url: "`+card.URL+`/card.json", headers: {X-Internal: agent-secret}, guardrails: [no-email]}
    checked: {grpc_url: "grpc://`+agent.addr+`", card_url: "`+card.URL+`/card.json", guardrails: [no-echo]}
    rpconly: {url: "`+card.URL+`/rpc"}
guardrails:
  no-email: {type: pii, detect: [email], action: redact, stages: [input]}
  no-echo: {type: regex, patterns: ['echo'], action: block, stages: [output]}
keys:
  - {name: a, key: `+devKey+`}
  - {name: b, key: `+guardKey+`}
`)
	// The gateway serves gRPC on its HTTP port, over HTTP/2 without TLS.
	gw := httptest.NewUnstartedServer(e.h)
	gw.Config.Protocols = new(http.Protocols)
	gw.Config.Protocols.SetHTTP1(true)
	gw.Config.Protocols.SetUnencryptedHTTP2(true)
	gw.Start()
	t.Cleanup(gw.Close)
	conn, err := grpc.NewClient(gw.Listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	svc, _ := a2apb.Service()
	as := func(key string) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		if key == "" {
			return ctx
		}
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)
	}
	msgs := func(method, js string) (in, out *dynamicpb.Message) {
		t.Helper()
		md := svc.Methods().ByName(protoreflect.Name(method))
		in, out = dynamicpb.NewMessage(md.Input()), dynamicpb.NewMessage(md.Output())
		if err := protojson.Unmarshal([]byte(js), in); err != nil {
			t.Fatal(err)
		}
		return in, out
	}
	call := func(key, method, js string) (map[string]any, error) {
		t.Helper()
		in, out := msgs(method, js)
		if err := conn.Invoke(as(key), a2aGRPCPath+method, in, out); err != nil {
			return nil, err
		}
		m, err := messageJSON(out)
		if err != nil {
			t.Fatal(err)
		}
		return m, nil
	}
	reason := func(err error) string {
		for _, d := range status.Convert(err).Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				return info.Reason
			}
		}
		return ""
	}
	send := func(tenant string) string {
		return `{"tenant":"` + tenant + `","message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"mail ann@example.com"}]}}`
	}

	// A message reaches the agent with the agent's tenant and
	// credentials, after the input guardrails.
	res, err := call(devKey, "SendMessage", send("planner"))
	if err != nil || res["task"].(map[string]any)["id"] != "grpc-task" {
		t.Fatalf("SendMessage = %v, %v", res, err)
	}
	got := agent.last()
	if strings.Contains(got, "ann@example.com") || !strings.Contains(got, "agent-secret") || strings.Contains(got, devKey) || !strings.Contains(got, `"tenant":"inner"`) {
		t.Fatalf("the agent got %s", got)
	}

	// The task is its caller's.
	if res, err := call(devKey, "GetTask", `{"tenant":"planner","id":"grpc-task"}`); err != nil || res["id"] != "grpc-task" {
		t.Fatalf("GetTask = %v, %v", res, err)
	}
	for _, method := range []string{"GetTask", "CancelTask"} {
		_, err := call(guardKey, method, `{"tenant":"planner","id":"grpc-task"}`)
		if status.Code(err) != codes.NotFound || reason(err) != "TASK_NOT_FOUND" {
			t.Errorf("another key: %s = %v", method, err)
		}
	}
	// The agent's own errors pass through, with their details.
	if _, err := call(devKey, "CancelTask", `{"tenant":"planner","id":"grpc-task"}`); status.Code(err) != codes.FailedPrecondition || reason(err) != "TASK_NOT_CANCELABLE" {
		t.Fatalf("CancelTask = %v", err)
	}

	// A stream's events pass through, and the tasks they name are recorded.
	in, _ := msgs("SendStreamingMessage", send("planner"))
	cs, err := conn.NewStream(as(devKey), &grpc.StreamDesc{ServerStreams: true}, a2aGRPCPath+"SendStreamingMessage")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SendMsg(in); err != nil {
		t.Fatal(err)
	}
	_ = cs.CloseSend()
	var events []string
	for {
		ev := dynamicpb.NewMessage(svc.Methods().ByName("SendStreamingMessage").Output())
		err := cs.RecvMsg(ev)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		raw, _ := protojson.Marshal(ev)
		events = append(events, string(raw))
	}
	if len(events) != 2 || !strings.Contains(events[1], "statusUpdate") {
		t.Fatalf("events = %v", events)
	}
	if _, err := call(devKey, "GetTask", `{"tenant":"planner","id":"stream-task"}`); err != nil {
		t.Fatalf("GetTask of a streamed task: %v", err)
	}
	if _, err := call(guardKey, "GetTask", `{"tenant":"planner","id":"stream-task"}`); status.Code(err) != codes.NotFound {
		t.Fatalf("another key's GetTask of a streamed task: %v", err)
	}

	// Output guardrails check results, and refuse streams.
	if _, err := call(devKey, "SendMessage", send("checked")); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "guardrail") {
		t.Fatalf("blocked output = %v", err)
	}
	in, _ = msgs("SendStreamingMessage", send("checked"))
	cs, _ = conn.NewStream(as(devKey), &grpc.StreamDesc{ServerStreams: true}, a2aGRPCPath+"SendStreamingMessage")
	_ = cs.SendMsg(in)
	_ = cs.CloseSend()
	if err := cs.RecvMsg(dynamicpb.NewMessage(svc.Methods().ByName("SendStreamingMessage").Output())); status.Code(err) != codes.FailedPrecondition || reason(err) != "UNSUPPORTED_OPERATION" {
		t.Fatalf("stream with output guardrails = %v", err)
	}

	// Refusals.
	before := agent.last()
	for name, tc := range map[string]struct {
		key, method, body string
		want              codes.Code
	}{
		"no key":          {"", "SendMessage", send("planner"), codes.Unauthenticated},
		"no tenant":       {devKey, "SendMessage", send(""), codes.InvalidArgument},
		"unknown agent":   {devKey, "SendMessage", send("nobody"), codes.NotFound},
		"not over gRPC":   {devKey, "SendMessage", send("rpconly"), codes.FailedPrecondition},
		"list tasks":      {devKey, "ListTasks", `{"tenant":"planner"}`, codes.FailedPrecondition},
		"extended card":   {devKey, "GetExtendedAgentCard", `{"tenant":"planner"}`, codes.Unimplemented},
		"push is off":     {devKey, "GetTaskPushNotificationConfig", `{"tenant":"planner","taskId":"grpc-task","id":"c1"}`, codes.FailedPrecondition},
		"unknown task":    {devKey, "GetTask", `{"tenant":"planner","id":"nope"}`, codes.NotFound},
		"another context": {guardKey, "SendMessage", `{"tenant":"planner","message":{"messageId":"m2","role":"ROLE_USER","contextId":"ctx-grpc-task","parts":[{"text":"hi"}]}}`, codes.NotFound},
	} {
		if _, err := call(tc.key, tc.method, tc.body); status.Code(err) != tc.want {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if agent.last() != before {
		t.Fatalf("a refused request reached the agent: %s", agent.last())
	}
	// Another service is not served.
	if err := conn.Invoke(as(devKey), "/other.Service/Do", dynamicpb.NewMessage(svc.Methods().ByName("GetTask").Input()), dynamicpb.NewMessage(svc.Methods().ByName("GetTask").Output())); status.Code(err) == codes.OK {
		t.Fatal("another service was served")
	}

	// The card names the gateway's gRPC address, and the agent as tenant.
	rec := do(e.h, "GET", "/a2a/planner/.well-known/agent-card.json", devKey, "")
	var c struct {
		SupportedInterfaces []map[string]any `json:"supportedInterfaces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || len(c.SupportedInterfaces) != 1 {
		t.Fatalf("card: %d %s", rec.Code, rec.Body)
	}
	if i := c.SupportedInterfaces[0]; i["url"] != "gw.example.com:443" || i["protocolBinding"] != "GRPC" || i["tenant"] != "planner" {
		t.Fatalf("card interface = %v", i)
	}
	// Without HTTP/2 the path says what it is.
	if rec := do(e.h, "POST", a2aGRPCPath+"GetTask", devKey, "{}"); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("gRPC path over HTTP/1.1: %d", rec.Code)
	}
}
