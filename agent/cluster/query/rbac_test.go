package query

import (
	"encoding/json"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

func TestRBACWhoCanOp(t *testing.T) {
	g := func(r string) schema.GroupVersionResource { return gvr("rbac.authorization.k8s.io", "v1", r) }
	objs := map[schema.GroupVersionResource][]runtime.Object{
		g("roles"): {&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "reader"},
			Rules: []rbacv1.PolicyRule{{Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"pods"}}}}},
		g("rolebindings"): {&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "reader"},
			RoleRef:  rbacv1.RoleRef{Kind: "Role", Name: "reader"},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "shop", Name: "web"}}}},
		g("clusterroles"):        {},
		g("clusterrolebindings"): {},
	}
	h, kube, _ := newHandler(t, objs)
	r := do(h, &protocol.Request{Op: protocol.OpRBACWhoCan, RBAC: &protocol.RBACQuery{Verb: "list", Resource: "pods", Namespace: "shop"}})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var res protocol.RBACResult
	if err := json.Unmarshal(r.Object, &res); err != nil {
		t.Fatal(err)
	}
	if res.Mode != protocol.RBACWhoCan || len(res.Subjects) != 1 || res.Subjects[0].Name != "web" {
		t.Errorf("result = %+v", res)
	}
	if len(kube.Actions()) != 0 {
		t.Errorf("rbac.whocan must not call the API (no SubjectAccessReview): %v", kube.Actions())
	}
	delete(objs, g("clusterrolebindings"))
	if r := do(h, &protocol.Request{Op: protocol.OpRBACWhoCan, RBAC: &protocol.RBACQuery{Verb: "list", Resource: "pods"}}); code(r) != protocol.CodeUnavailable {
		t.Errorf("RBAC not cached: %+v", r.Error)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpRBACWhoCan}); code(r) != protocol.CodeBadRequest {
		t.Errorf("no query: %+v", r.Error)
	}
}
