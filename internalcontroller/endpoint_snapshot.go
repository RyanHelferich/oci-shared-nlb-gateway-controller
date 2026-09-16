package internalcontroller

import (
	"context"
	"fmt"
	"time"

	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// endpointSnapshot is refreshed on every reconciliation. Bulk reads bound API
// traffic independently of binding count; it is never retained across retries.
type endpointSnapshot struct {
	services map[client.ObjectKey]core.Service
	pods     map[client.ObjectKey]core.Pod
	nodes    map[client.ObjectKey]core.Node
	slices   []discovery.EndpointSlice
}

func newEndpointSnapshot(ctx context.Context, reader client.Reader, namespace string) (*endpointSnapshot, error) {
	var services core.ServiceList
	var pods core.PodList
	var nodes core.NodeList
	var slices discovery.EndpointSliceList
	for index, target := range []client.ObjectList{&services, &slices, &pods, &nodes} {
		options := []client.ListOption{}
		if _, clusterScoped := target.(*core.NodeList); !clusterScoped {
			options = append(options, client.InNamespace(namespace))
		}
		started := time.Now()
		err := reader.List(ctx, target, options...)
		snapshotReadDuration.WithLabelValues([]string{"services", "endpointslices", "pods", "nodes"}[index]).Observe(time.Since(started).Seconds())
		if err != nil {
			return nil, &EndpointReadError{Err: err}
		}
	}
	snapshot := &endpointSnapshot{services: map[client.ObjectKey]core.Service{}, pods: map[client.ObjectKey]core.Pod{}, nodes: map[client.ObjectKey]core.Node{}, slices: slices.Items}
	for _, item := range services.Items {
		snapshot.services[client.ObjectKeyFromObject(&item)] = item
	}
	for _, item := range pods.Items {
		snapshot.pods[client.ObjectKeyFromObject(&item)] = item
	}
	for _, item := range nodes.Items {
		snapshot.nodes[client.ObjectKeyFromObject(&item)] = item
	}
	return snapshot, nil
}

func (s *endpointSnapshot) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch target := obj.(type) {
	case *core.Service:
		item, ok := s.services[key]
		if !ok {
			return apierrors.NewNotFound(core.Resource("services"), key.Name)
		}
		*target = *item.DeepCopy()
	case *core.Pod:
		item, ok := s.pods[key]
		if !ok {
			return apierrors.NewNotFound(core.Resource("pods"), key.Name)
		}
		*target = *item.DeepCopy()
	case *core.Node:
		item, ok := s.nodes[key]
		if !ok {
			return apierrors.NewNotFound(core.Resource("nodes"), key.Name)
		}
		*target = *item.DeepCopy()
	default:
		return fmt.Errorf("unsupported endpoint snapshot Get type %T", obj)
	}
	return nil
}

func (s *endpointSnapshot) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	options := &client.ListOptions{}
	options.ApplyOptions(opts)
	if options.FieldSelector != nil && !options.FieldSelector.Empty() {
		return fmt.Errorf("endpoint snapshot does not support field selectors")
	}
	matches := func(obj client.Object) bool {
		return (options.Namespace == "" || options.Namespace == obj.GetNamespace()) && (options.LabelSelector == nil || options.LabelSelector.Matches(labels.Set(obj.GetLabels())))
	}
	switch target := list.(type) {
	case *discovery.EndpointSliceList:
		target.Items = nil
		for _, item := range s.slices {
			if matches(&item) {
				target.Items = append(target.Items, *item.DeepCopy())
			}
		}
	case *core.NodeList:
		target.Items = nil
		for _, item := range s.nodes {
			if matches(&item) {
				target.Items = append(target.Items, *item.DeepCopy())
			}
		}
	default:
		return fmt.Errorf("unsupported endpoint snapshot List type %T", list)
	}
	return nil
}
