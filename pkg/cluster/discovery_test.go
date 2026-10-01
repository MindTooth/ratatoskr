package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResolve(t *testing.T) {
	sub := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "gitops", "namespace": "openshift-gitops-operator"},
		"spec": map[string]any{
			"name": "openshift-gitops-operator", "channel": "gitops-1.20",
			"source": "redhat-operators", "sourceNamespace": "openshift-marketplace",
		},
	}}
	manifest := unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"packageName": "openshift-gitops-operator", "catalogSource": "redhat-operators",
			"catalogSourceNamespace": "openshift-marketplace", "defaultChannel": "gitops-1.21",
			"channels": []any{
				map[string]any{"name": "gitops-1.20", "currentCSVDesc": map[string]any{"version": "1.20.8"}},
				map[string]any{"name": "gitops-1.21", "currentCSVDesc": map[string]any{"version": "1.21.2"}},
			},
		},
	}}

	got, ok := Resolve(sub, []unstructured.Unstructured{manifest})
	if !ok {
		t.Fatal("expected subscription to resolve")
	}
	if got.CurrentChannelVersion != "1.20.8" {
		t.Fatalf("current channel version = %q, want 1.20.8", got.CurrentChannelVersion)
	}
	if got.DefaultChannel != "gitops-1.21" || got.DefaultChannelVersion != "1.21.2" {
		t.Fatalf("default = %s/%s, want gitops-1.21/1.21.2", got.DefaultChannel, got.DefaultChannelVersion)
	}
}

func TestResolveRejectsDifferentCatalog(t *testing.T) {
	sub := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"name": "example", "channel": "stable", "source": "redhat-operators"},
	}}
	manifest := unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"packageName": "example", "catalogSource": "community-operators"},
	}}
	if _, ok := Resolve(sub, []unstructured.Unstructured{manifest}); ok {
		t.Fatal("expected different catalog source not to resolve")
	}
}
