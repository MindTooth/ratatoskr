// Package cluster discovers OLM subscription version metadata from a cluster.
package cluster

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	subscriptionGVR = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "subscriptions"}
	packageManifestGVR = schema.GroupVersionResource{Group: "packages.operators.coreos.com", Version: "v1", Resource: "packagemanifests"}
)

// SubscriptionVersion describes the configured channel and catalog default for an OLM subscription.
type SubscriptionVersion struct {
	Namespace             string `json:"namespace"`
	Name                  string `json:"name"`
	Package               string `json:"package"`
	CatalogSource         string `json:"catalogSource"`
	CurrentChannel        string `json:"currentChannel"`
	CurrentChannelVersion string `json:"currentChannelVersion"`
	DefaultChannel        string `json:"defaultChannel"`
	DefaultChannelVersion string `json:"defaultChannelVersion"`
}

// Discover lists OLM subscriptions and resolves channel heads from PackageManifests.
func Discover(ctx context.Context, client dynamic.Interface) ([]SubscriptionVersion, error) {
	subscriptions, err := client.Resource(subscriptionGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	manifests, err := client.Resource(packageManifestGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list package manifests: %w", err)
	}

	result := make([]SubscriptionVersion, 0, len(subscriptions.Items))
	for i := range subscriptions.Items {
		resolved, ok := Resolve(&subscriptions.Items[i], manifests.Items)
		if ok {
			result = append(result, resolved)
		}
	}
	return result, nil
}

// Resolve matches a subscription to its PackageManifest and resolves the heads of
// the configured channel and catalog default channel.
func Resolve(subscription *unstructured.Unstructured, manifests []unstructured.Unstructured) (SubscriptionVersion, bool) {
	packageName, _, _ := unstructured.NestedString(subscription.Object, "spec", "name")
	source, _, _ := unstructured.NestedString(subscription.Object, "spec", "source")
	sourceNamespace, _, _ := unstructured.NestedString(subscription.Object, "spec", "sourceNamespace")
	channel, _, _ := unstructured.NestedString(subscription.Object, "spec", "channel")
	if packageName == "" || source == "" || channel == "" {
		return SubscriptionVersion{}, false
	}

	for i := range manifests {
		manifest := &manifests[i]
		manifestPackage, _, _ := unstructured.NestedString(manifest.Object, "status", "packageName")
		manifestSource, _, _ := unstructured.NestedString(manifest.Object, "status", "catalogSource")
		manifestSourceNamespace, _, _ := unstructured.NestedString(manifest.Object, "status", "catalogSourceNamespace")
		if manifestPackage != packageName || manifestSource != source || (sourceNamespace != "" && manifestSourceNamespace != sourceNamespace) {
			continue
		}

		defaultChannel, _, _ := unstructured.NestedString(manifest.Object, "status", "defaultChannel")
		channels, _, _ := unstructured.NestedSlice(manifest.Object, "status", "channels")
		currentVersion := channelVersion(channels, channel)
		defaultVersion := channelVersion(channels, defaultChannel)
		if currentVersion == "" || defaultChannel == "" || defaultVersion == "" {
			return SubscriptionVersion{}, false
		}
		return SubscriptionVersion{
			Namespace: subscription.GetNamespace(), Name: subscription.GetName(),
			Package: packageName, CatalogSource: source,
			CurrentChannel: channel, CurrentChannelVersion: currentVersion,
			DefaultChannel: defaultChannel, DefaultChannelVersion: defaultVersion,
		}, true
	}
	return SubscriptionVersion{}, false
}

func channelVersion(channels []any, name string) string {
	for _, raw := range channels {
		channel, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		channelName, _, _ := unstructured.NestedString(channel, "name")
		if channelName != name {
			continue
		}
		version, _, _ := unstructured.NestedString(channel, "currentCSVDesc", "version")
		return version
	}
	return ""
}
