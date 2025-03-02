package dao

import (
	"context"
	"fmt"
	"io"

	"github.com/derailed/k9s/internal"
	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/render"
	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/drain"
	"k8s.io/kubectl/pkg/scheme"
	mv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

var (
	_ Accessor       = (*Node)(nil)
	_ NodeMaintainer = (*Node)(nil)
)

// NodeMetricsFunc retrieves node metrics.
type NodeMetricsFunc func() (*mv1beta1.NodeMetricsList, error)

// Node represents a node model.
type Node struct {
	Resource
}

// ToggleCordon toggles cordon/uncordon a node.
func (n *Node) ToggleCordon(path string, cordon bool) error {
	o, err := n.Get(context.Background(), path)
	if err != nil {
		return err
	}

	h, err := drain.NewCordonHelperFromRuntimeObject(o, scheme.Scheme, n.gvr.GVK())
	if err != nil {
		return err
	}

	if !h.UpdateIfRequired(cordon) {
		if cordon {
			return fmt.Errorf("node is already cordoned")
		}
		return fmt.Errorf("node is already uncordoned")
	}
	err, patchErr := h.PatchOrReplace(n.Factory.Client().DialOrDie())
	if patchErr != nil {
		return patchErr
	}
	if err != nil {
		return err
	}

	return nil
}

func (o DrainOptions) toDrainHelper(k kubernetes.Interface, w io.Writer) drain.Helper {
	return drain.Helper{
		Client:              k,
		GracePeriodSeconds:  o.GracePeriodSeconds,
		Timeout:             o.Timeout,
		DeleteLocalData:     o.DeleteLocalData,
		IgnoreAllDaemonSets: o.IgnoreAllDaemonSets,
		Out:                 w,
		ErrOut:              w,
	}
}

// Drain drains a node.
func (n *Node) Drain(path string, opts DrainOptions, w io.Writer) error {
	_ = n.ToggleCordon(path, true)

	h := opts.toDrainHelper(n.Factory.Client().DialOrDie(), w)
	dd, errs := h.GetPodsForDeletion(path)
	if len(errs) != 0 {
		for _, e := range errs {
			if _, err := h.ErrOut.Write([]byte(e.Error() + "\n")); err != nil {
				return err
			}
		}
		return errs[0]
	}

	if err := h.DeleteOrEvictPods(dd.Pods()); err != nil {
		return err
	}
	fmt.Fprintf(h.Out, "Node %s drained!", path)

	return nil
}

// Get returns a node resource.
func (n *Node) Get(_ context.Context, path string) (runtime.Object, error) {
	return FetchNode(n.Factory, path)
}

// List returns a collection of node resources.
func (n *Node) List(ctx context.Context, ns string) ([]runtime.Object, error) {
	labels, ok := ctx.Value(internal.KeyLabels).(string)
	if !ok {
		log.Warn().Msgf("No label selector found in context")
	}

	var (
		nmx *mv1beta1.NodeMetricsList
		err error
	)
	if withMx, ok := ctx.Value(internal.KeyWithMetrics).(bool); withMx || !ok {
		if nmx, err = client.DialMetrics(n.Client()).FetchNodesMetrics(); err != nil {
			log.Warn().Err(err).Msgf("No node metrics")
		}
	}

	nn, err := FetchNodes(n.Factory, labels)
	if err != nil {
		return nil, err
	}
	oo := make([]runtime.Object, len(nn.Items))
	for i, no := range nn.Items {
		o, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&nn.Items[i])
		if err != nil {
			return nil, err
		}
		oo[i] = &render.NodeWithMetrics{
			Raw: &unstructured.Unstructured{Object: o},
			MX:  nodeMetricsFor(MetaFQN(no.ObjectMeta), nmx),
		}
	}

	return oo, nil
}

// ----------------------------------------------------------------------------
// Helpers...

// FetchNode retrieves a node.
func FetchNode(f Factory, path string) (*v1.Node, error) {
	auth, err := f.Client().CanI("", "v1/nodes", []string{"get"})
	if err != nil {
		return nil, err
	}
	if !auth {
		return nil, fmt.Errorf("user is not authorized to list nodes")
	}

	return f.Client().DialOrDie().CoreV1().Nodes().Get(path, metav1.GetOptions{})
}

// FetchNodes retrieves all nodes.
func FetchNodes(f Factory, labelsSel string) (*v1.NodeList, error) {
	auth, err := f.Client().CanI("", "v1/nodes", []string{client.ListVerb})
	if err != nil {
		return nil, err
	}
	if !auth {
		return nil, fmt.Errorf("user is not authorized to list nodes")
	}

	return f.Client().DialOrDie().CoreV1().Nodes().List(metav1.ListOptions{
		LabelSelector: labelsSel,
	})
}

func nodeMetricsFor(fqn string, mmx *mv1beta1.NodeMetricsList) *mv1beta1.NodeMetrics {
	if mmx == nil {
		return nil
	}
	for _, mx := range mmx.Items {
		if MetaFQN(mx.ObjectMeta) == fqn {
			return &mx
		}
	}
	return nil
}
