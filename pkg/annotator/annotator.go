package annotator

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/retry"
)

type AnnotateError struct {
	Kind             string
	Namespace        string
	Name             string
	Err              error
}

func (err AnnotateError) Unwrap() error {
	return err.Err
}

func (err AnnotateError) Error() string {
	if err.Namespace == "" {
		return fmt.Sprintf("%s/%s: %s", err.Kind, err.Name, err.Unwrap().Error())
	}
	return fmt.Sprintf("%s:%s/%s: %s", err.Namespace, err.Kind, err.Name, err.Unwrap().Error())
}

type AnnotationError []AnnotateError

func (err AnnotationError) Error() string {
	var errs []string
	for _, e := range err {
		errs = append(errs, e.Err.Error())
	}
	return strings.Join(errs, "; ")
}

type Annotator struct {
	discoveryClient discovery.DiscoveryInterface
	dynamicClient dynamic.Interface
}

func NewAnnotator(discoveryClient discovery.DiscoveryInterface, dynamicClient dynamic.Interface) *Annotator {
	return &Annotator{discoveryClient, dynamicClient}
}

func (a *Annotator) Annotate(objs []unstructured.Unstructured, defaultNamespace, annotation, value string) error {
	restMap, err := buildDiscoveryRestMapper(a.discoveryClient)
	if err != nil {
		return err
	}
	patch := []byte(`{"metadata":{"annotations":{"`+annotation+`":"`+value+`"}}}`)
	var errs AnnotationError
	for _, obj := range objs {
		namespace := obj.GetNamespace()
		mapping, err := restMap.RESTMapping(obj.GroupVersionKind().GroupKind(), obj.GroupVersionKind().Version)
		if err != nil {
			errs = append(errs, AnnotateError{obj.GroupVersionKind().Kind, namespace, obj.GetName(), err})
			continue
		}
		if isNamespaced(mapping) && namespace == "" {
			namespace = defaultNamespace
		}
		if _, err := a.dynamicClient.Resource(mapping.Resource).Namespace(namespace).Patch(obj.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			errs = append(errs, AnnotateError{obj.GroupVersionKind().Kind, namespace, obj.GetName(), err})
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func (a *Annotator) OneOfResourcesHasAnnotationValueOrNil(objs []unstructured.Unstructured, defaultNamespace, annotation, value string) (bool, string, error) {
	restMap, err := buildDiscoveryRestMapper(a.discoveryClient)
	if err != nil {
		return false, "", err
	}
	var errs AnnotationError
	for _, obj := range objs {
		namespace := obj.GetNamespace()
		mapping, err := restMap.RESTMapping(obj.GroupVersionKind().GroupKind(), obj.GroupVersionKind().Version)
		if err != nil {
			errs = append(errs, AnnotateError{obj.GroupVersionKind().Kind, namespace, obj.GetName(), err})
			continue
		}
		if isNamespaced(mapping) && namespace == "" {
			namespace = defaultNamespace
		}
		{
			var res *unstructured.Unstructured
			var err error
			wait.ExponentialBackoff(retry.DefaultBackoff, func() (bool, error) {
				res, err = a.dynamicClient.Resource(mapping.Resource).Namespace(namespace).Get(obj.GetName(), metav1.GetOptions{})
				// All these errors indicate a transient error that should
				// be retried.
				if net.IsConnectionReset(err) || errors.IsInternalError(err) || errors.IsTimeout(err) || errors.IsTooManyRequests(err) {
					return false, nil
				}
				// Checks for a Retry-After header, the presence of this
				// header is an explicit signal we should retry.
				if _, shouldRetry := errors.SuggestsClientDelay(err); shouldRetry {
					return false, nil
				}
				if err != nil {
					return false, err
				}
				return true, nil
			})
			if v, ok := res.GetAnnotations()[annotation]; ok {
				return v == value, v, nil
			}
		}
	}
	if len(errs) > 0 {
		return false, "", errs
	}
	return true, "", nil
}

func buildDiscoveryRestMapper(client discovery.DiscoveryInterface) (meta.RESTMapper, error) {
	groupResources, err := restmapper.GetAPIGroupResources(client)
	if err != nil {
		return nil, err
	}
	return restmapper.NewDiscoveryRESTMapper(groupResources), nil
}

func isNamespaced(mapping *meta.RESTMapping) bool {
	return mapping.Scope.Name() == meta.RESTScopeNameNamespace
}
