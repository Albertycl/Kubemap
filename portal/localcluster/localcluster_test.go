package localcluster

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"kubemap/agent/cluster/cache"
	"kubemap/pkg/protocol"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The real in-process agent, attached to a real Hub, checks the tokens of
// two node agents on two different nodes with TokenReview (fake Kubernetes
// API), and the Hub tracks both.
func TestInProcessAgentAcceptsNodesOnSeveralNodes(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.EnsureCluster(ctx, ClusterID, ClusterName); err != nil {
		t.Fatal(err)
	}

	kube := kubefake.NewClientset()
	kube.PrependReactor("create", "tokenreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		tok := a.(clienttesting.CreateAction).GetObject().(*authv1.TokenReview).Spec.Token
		node, ok := strings.CutPrefix(tok, "token-for-")
		if !ok {
			return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: false}}, nil
		}
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true, Audiences: []string{protocol.NodeTokenAudience},
			User: authv1.UserInfo{Username: protocol.NodeAgentUsername, Extra: map[string]authv1.ExtraValue{
				"authentication.kubernetes.io/pod-name":  {"kubemap-node-agent-" + node},
				"authentication.kubernetes.io/pod-uid":   {"uid-" + node},
				"authentication.kubernetes.io/node-name": {node},
			}},
		}}, nil
	})
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{{Version: "v1", Resource: "namespaces"}: "NamespaceList"})
	c := cache.New(kube, dyn, kube.Discovery(), cache.Options{Logger: log})
	agent := New(c, kube, dyn, "kubemap-system", "v1.34.0", log)

	hub := tunnel.New(tunnel.Config{Store: st, Log: log})
	t.Cleanup(func() { hub.Close(time.Second) })
	actx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	_, detach, err := hub.AttachLocal(actx, ClusterID, "test", agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(detach)
	if !hub.Connected(ClusterID) {
		t.Fatal("the in-process agent is not attached")
	}

	mux := http.NewServeMux()
	hub.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/connect"

	register := func(node, token string) *websocket.Conn {
		t.Helper()
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		defer dcancel()
		ws, _, err := websocket.Dial(dctx, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ws.CloseNow() })
		b, err := protocol.Encode("reg-"+node, &protocol.Register{Role: protocol.RoleNode, AgentVersion: "test", ClusterID: ClusterID, NodeName: node, ServiceAccountToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.Write(dctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
		_, data, err := ws.Read(dctx)
		if err != nil {
			t.Fatal(err)
		}
		env, _, err := protocol.Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		if env.Type != protocol.TypeRegisterAck {
			t.Fatalf("%s: got %s, want register ack", node, env.Type)
		}
		return ws
	}
	register("worker-1", "token-for-worker-1")
	register("worker-2", "token-for-worker-2")
	waitFor(t, "both nodes tracked", func() bool {
		return hub.NodeConnected(ClusterID, "worker-1") && hub.NodeConnected(ClusterID, "worker-2")
	})

	// worker-1's token cannot be used to register as worker-2's name.
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	ws, _, err := websocket.Dial(dctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	b, _ := protocol.Encode("x", &protocol.Register{Role: protocol.RoleNode, ClusterID: ClusterID, NodeName: "worker-3", ServiceAccountToken: "token-for-worker-1"})
	ws.Write(dctx, websocket.MessageText, b)
	_, data, err := ws.Read(dctx)
	if err != nil {
		t.Fatal(err)
	}
	if env, _, _ := protocol.Decode(data); env == nil || env.Type != protocol.TypeError {
		t.Fatalf("a token for another node was accepted: %s", data)
	}
	if hub.NodeConnected(ClusterID, "worker-3") {
		t.Fatal("worker-3 tracked with worker-1's token")
	}

	// A query through the Hub reaches the in-process handler.
	resp, err := hub.Request(ctx, ClusterID, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "namespaces"})
	if err != nil || resp == nil {
		t.Fatalf("request: %v %+v", err, resp)
	}
}
