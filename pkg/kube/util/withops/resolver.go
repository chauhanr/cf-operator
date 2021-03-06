package withops

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"reflect"
	"strings"

	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bdm "code.cloudfoundry.org/cf-operator/pkg/bosh/manifest"
	bdv1 "code.cloudfoundry.org/cf-operator/pkg/kube/apis/boshdeployment/v1alpha1"
	"code.cloudfoundry.org/quarks-utils/pkg/ctxlog"
	"code.cloudfoundry.org/quarks-utils/pkg/logger"
	"code.cloudfoundry.org/quarks-utils/pkg/names"
	"code.cloudfoundry.org/quarks-utils/pkg/versionedsecretstore"
)

// DomainNameService consumer interface
type DomainNameService interface {
	// HeadlessServiceName constructs the headless service name for the instance group.
	HeadlessServiceName(instanceGroupName string) string
}

// Resolver resolves references from bdpl CR to a BOSH manifest
type Resolver struct {
	client               client.Client
	versionedSecretStore versionedsecretstore.VersionedSecretStore
	newInterpolatorFunc  NewInterpolatorFunc
	newDNSFunc           NewDNSFunc
}

// NewInterpolatorFunc returns a fresh Interpolator
type NewInterpolatorFunc func() Interpolator

// NewDNSFunc returns a dns client for the manifest
type NewDNSFunc func(deploymentName string, m bdm.Manifest) (DomainNameService, error)

// NewResolver constructs a resolver
func NewResolver(client client.Client, f NewInterpolatorFunc, dns NewDNSFunc) *Resolver {
	return &Resolver{
		client:               client,
		newInterpolatorFunc:  f,
		newDNSFunc:           dns,
		versionedSecretStore: versionedsecretstore.NewVersionedSecretStore(client),
	}
}

// Manifest returns manifest and a list of implicit variables referenced by our bdpl CRD
// The resulting manifest has variables interpolated and ops files applied.
// It is the 'with-ops' manifest.
func (r *Resolver) Manifest(ctx context.Context, bdpl *bdv1.BOSHDeployment, namespace string) (*bdm.Manifest, []string, error) {
	interpolator := r.newInterpolatorFunc()
	spec := bdpl.Spec
	var (
		m   string
		err error
	)

	m, err = r.resourceData(ctx, namespace, spec.Manifest.Type, spec.Manifest.Name, bdv1.ManifestSpecName)
	if err != nil {
		return nil, []string{}, errors.Wrapf(err, "Interpolation failed for bosh deployment %s", bdpl.GetName())
	}

	// Interpolate manifest with ops
	ops := spec.Ops

	for _, op := range ops {
		opsData, err := r.resourceData(ctx, namespace, op.Type, op.Name, bdv1.OpsSpecName)
		if err != nil {
			return nil, []string{}, errors.Wrapf(err, "Interpolation failed for bosh deployment %s", bdpl.GetName())
		}
		err = interpolator.BuildOps([]byte(opsData))
		if err != nil {
			return nil, []string{}, errors.Wrapf(err, "Interpolation failed for bosh deployment %s", bdpl.GetName())
		}
	}

	bytes := []byte(m)
	if len(ops) != 0 {
		bytes, err = interpolator.Interpolate([]byte(m))
		if err != nil {
			return nil, []string{}, errors.Wrapf(err, "Failed to interpolate %#v in interpolation task", m)
		}
	}

	// Reload the manifest after interpolation, and apply implicit variables
	manifest, err := bdm.LoadYAML(bytes)
	if err != nil {
		return nil, []string{}, errors.Wrapf(err, "Loading yaml failed in interpolation task after applying ops %#v", m)
	}

	// Interpolate implicit variables
	vars, err := manifest.ImplicitVariables()
	if err != nil {
		return nil, []string{}, errors.Wrapf(err, "failed to list implicit variables")
	}

	varSecrets := make([]string, len(vars))
	for i, v := range vars {
		varKeyName := ""
		varSecretName := ""
		if strings.Contains(v, "/") {
			parts := strings.Split(v, "/")
			if len(parts) != 2 {
				return nil, []string{}, fmt.Errorf("expected one / separator for implicit variable/key name, have %d", len(parts))
			}

			varSecretName = names.DeploymentSecretName(names.DeploymentSecretTypeVariable, bdpl.GetName(), parts[0])
			varKeyName = parts[1]
		} else {
			varKeyName = bdv1.ImplicitVariableKeyName
			varSecretName = names.DeploymentSecretName(names.DeploymentSecretTypeVariable, bdpl.GetName(), v)
		}

		varData, err := r.resourceData(ctx, namespace, bdv1.SecretReference, varSecretName, varKeyName)
		if err != nil {
			return nil, varSecrets, errors.Wrapf(err, "failed to load secret for variable '%s'", v)
		}

		varSecrets[i] = varSecretName
		manifest = r.replaceVar(manifest, v, varData)
	}

	// Apply addons
	log := ctxlog.ExtractLogger(ctx)
	err = manifest.ApplyAddons(logger.TraceFilter(log, "manifest-addons"))
	if err != nil {
		return nil, varSecrets, errors.Wrapf(err, "failed to apply addons")
	}

	dns, err := r.newDNSFunc(bdpl.Name, *manifest)
	if err != nil {
		return nil, nil, err
	}
	manifest.ApplyUpdateBlock(dns)

	return manifest, varSecrets, err
}

// ManifestDetailed returns manifest and a list of implicit variables referenced by our bdpl CRD
// The resulting manifest has variables interpolated and ops files applied.
// It is the 'with-ops' manifest. This variant processes each ops file individually, so it's more debuggable - but slower.
func (r *Resolver) ManifestDetailed(ctx context.Context, bdpl *bdv1.BOSHDeployment, namespace string) (*bdm.Manifest, []string, error) {
	spec := bdpl.Spec
	var (
		m   string
		err error
	)

	m, err = r.resourceData(ctx, namespace, spec.Manifest.Type, spec.Manifest.Name, bdv1.ManifestSpecName)
	if err != nil {
		return nil, []string{}, errors.Wrapf(err, "Interpolation failed for bosh deployment %s", bdpl.GetName())
	}

	// Interpolate manifest with ops
	ops := spec.Ops
	bytes := []byte(m)

	for _, op := range ops {
		interpolator := r.newInterpolatorFunc()

		opsData, err := r.resourceData(ctx, namespace, op.Type, op.Name, bdv1.OpsSpecName)
		if err != nil {
			return nil, []string{}, errors.Wrapf(err, "Failed to get resource data for interpolation of bosh deployment '%s' and ops '%s'", bdpl.GetName(), op.Name)
		}
		err = interpolator.BuildOps([]byte(opsData))
		if err != nil {
			return nil, []string{}, errors.Wrapf(err, "Interpolation failed for bosh deployment '%s' and ops '%s'", bdpl.GetName(), op.Name)
		}

		bytes, err = interpolator.Interpolate(bytes)
		if err != nil {
			return nil, []string{}, errors.Wrapf(err, "Failed to interpolate ops '%s' for manifest '%s'", op.Name, bdpl.Name)
		}
	}

	// Reload the manifest after interpolation, and apply implicit variables
	manifest, err := bdm.LoadYAML(bytes)
	if err != nil {
		return nil, []string{}, errors.Wrapf(err, "Loading yaml failed in interpolation task after applying ops %#v", m)
	}

	// Interpolate implicit variables
	vars, err := manifest.ImplicitVariables()
	if err != nil {
		return nil, []string{}, errors.Wrapf(err, "failed to list implicit variables")
	}

	varSecrets := make([]string, len(vars))
	for i, v := range vars {
		varKeyName := ""
		varSecretName := ""
		if strings.Contains(v, "/") {
			parts := strings.Split(v, "/")
			if len(parts) != 2 {
				return nil, []string{}, fmt.Errorf("expected one / separator for implicit variable/key name, have %d", len(parts))
			}

			varSecretName = names.DeploymentSecretName(names.DeploymentSecretTypeVariable, bdpl.GetName(), parts[0])
			varKeyName = parts[1]
		} else {
			varKeyName = bdv1.ImplicitVariableKeyName
			varSecretName = names.DeploymentSecretName(names.DeploymentSecretTypeVariable, bdpl.GetName(), v)
		}

		varData, err := r.resourceData(ctx, namespace, bdv1.SecretReference, varSecretName, varKeyName)
		if err != nil {
			return nil, varSecrets, errors.Wrapf(err, "failed to load secret for variable '%s'", v)
		}

		varSecrets[i] = varSecretName
		manifest = r.replaceVar(manifest, v, varData)
	}

	// Apply addons
	log := ctxlog.ExtractLogger(ctx)
	err = manifest.ApplyAddons(logger.TraceFilter(log, "detailed-manifest-addons"))
	if err != nil {
		return nil, varSecrets, errors.Wrapf(err, "failed to apply addons")
	}

	dns, err := r.newDNSFunc(bdpl.Name, *manifest)
	if err != nil {
		return nil, nil, err
	}
	manifest.ApplyUpdateBlock(dns)

	return manifest, varSecrets, err
}

func (r *Resolver) replaceVar(manifest *bdm.Manifest, name, value string) *bdm.Manifest {
	original := reflect.ValueOf(manifest)
	replaced := reflect.New(original.Type()).Elem()

	r.replaceVarRecursive(replaced, original, name, value)

	return replaced.Interface().(*bdm.Manifest)
}
func (r *Resolver) replaceVarRecursive(copy, v reflect.Value, varName, varValue string) {
	switch v.Kind() {
	case reflect.Ptr:
		if !v.Elem().IsValid() {
			return
		}
		copy.Set(reflect.New(v.Elem().Type()))
		r.replaceVarRecursive(copy.Elem(), reflect.Indirect(v), varName, varValue)

	case reflect.Interface:
		originalValue := v.Elem()
		if !originalValue.IsValid() {
			return
		}
		copyValue := reflect.New(originalValue.Type()).Elem()
		r.replaceVarRecursive(copyValue, originalValue, varName, varValue)
		copy.Set(copyValue)

	case reflect.Struct:
		deepCopy := v.MethodByName("DeepCopy")
		if (deepCopy != reflect.Value{}) {
			values := deepCopy.Call([]reflect.Value{})
			copy.Set(values[0])
		}
		for i := 0; i < v.NumField(); i++ {
			r.replaceVarRecursive(copy.Field(i), v.Field(i), varName, varValue)
		}

	case reflect.Slice:
		copy.Set(reflect.MakeSlice(v.Type(), v.Len(), v.Cap()))
		for i := 0; i < v.Len(); i++ {
			r.replaceVarRecursive(copy.Index(i), v.Index(i), varName, varValue)
		}

	case reflect.Map:
		copy.Set(reflect.MakeMap(v.Type()))
		for _, key := range v.MapKeys() {
			originalValue := v.MapIndex(key)
			copyValue := reflect.New(originalValue.Type()).Elem()
			r.replaceVarRecursive(copyValue, originalValue, varName, varValue)
			copy.SetMapIndex(key, copyValue)
		}

	case reflect.String:
		if copy.CanSet() {
			replaced := strings.Replace(v.String(), fmt.Sprintf("((%s))", varName), varValue, -1)
			copy.SetString(replaced)
		}
	default:
		if copy.CanSet() {
			copy.Set(v)
		}
	}
}

// resourceData resolves different manifest reference types and returns the resource's data
func (r *Resolver) resourceData(ctx context.Context, namespace string, resType bdv1.ReferenceType, name string, key string) (string, error) {
	var (
		data string
		ok   bool
	)

	switch resType {
	case bdv1.ConfigMapReference:
		opsConfig := &corev1.ConfigMap{}
		err := r.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, opsConfig)
		if err != nil {
			return data, errors.Wrapf(err, "failed to retrieve %s from configmap '%s/%s' via client.Get", key, namespace, name)
		}
		data, ok = opsConfig.Data[key]
		if !ok {
			return data, fmt.Errorf("configMap '%s/%s' doesn't contain key %s", namespace, name, key)
		}
	case bdv1.SecretReference:
		opsSecret := &corev1.Secret{}
		err := r.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, opsSecret)
		if err != nil {
			return data, errors.Wrapf(err, "failed to retrieve %s from secret '%s/%s' via client.Get", key, namespace, name)
		}
		encodedData, ok := opsSecret.Data[key]
		if !ok {
			return data, fmt.Errorf("secret '%s/%s' doesn't contain key %s", namespace, name, key)
		}
		data = string(encodedData)
	case bdv1.URLReference:
		httpResponse, err := http.Get(name)
		if err != nil {
			return data, errors.Wrapf(err, "failed to resolve %s from url '%s' via http.Get", key, name)
		}
		body, err := ioutil.ReadAll(httpResponse.Body)
		if err != nil {
			return data, errors.Wrapf(err, "failed to read %s response body '%s' via ioutil", key, name)
		}
		data = string(body)
	default:
		return data, fmt.Errorf("unrecognized %s ref type %s", key, name)
	}

	return data, nil
}
