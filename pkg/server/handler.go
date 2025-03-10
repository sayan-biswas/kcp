/*
Copyright 2021 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/emicklei/go-restful"
	"github.com/kcp-dev/logicalcluster"
	jwt2 "gopkg.in/square/go-jose.v2/jwt"

	apiextensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	kaudit "k8s.io/apiserver/pkg/audit"
	apiserverdiscovery "k8s.io/apiserver/pkg/endpoints/discovery"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/genericcontrolplane"
	"k8s.io/kubernetes/pkg/genericcontrolplane/aggregator"

	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	tenancyv1beta1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1beta1"
)

var (
	reClusterName = regexp.MustCompile(`^([a-z]([a-z0-9-]{0,61}[a-z0-9])?:)*[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

	errorScheme = runtime.NewScheme()
	errorCodecs = serializer.NewCodecFactory(errorScheme)
)

func init() {
	errorScheme.AddUnversionedTypes(metav1.Unversioned,
		&metav1.Status{},
	)
}

const (
	passthroughHeader   = "X-Kcp-Api-V1-Discovery-Passthrough"
	workspaceAnnotation = "tenancy.kcp.dev/workspace"
)

type (
	acceptHeaderContextKeyType int
	userAgentContextKeyType    int
)

const (
	// clusterKey is the context key for the request namespace.
	acceptHeaderContextKey acceptHeaderContextKeyType = iota

	// userAgentContextKey is the context key for the request user-agent.
	userAgentContextKey userAgentContextKeyType = iota
)

// WithAcceptHeader makes the Accept header available for code in the handler chain. It is needed for
// Wildcard requests, when finding the CRD with a common schema. For PartialObjectMeta requests we cand
// weaken the schema requirement and allow different schemas across workspaces.
func WithAcceptHeader(apiHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := context.WithValue(req.Context(), acceptHeaderContextKey, req.Header.Get("Accept"))
		apiHandler.ServeHTTP(w, req.WithContext(ctx))
	})
}

func WithUserAgent(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := context.WithValue(req.Context(), userAgentContextKey, req.Header.Get("User-Agent"))
		handler.ServeHTTP(w, req.WithContext(ctx))
	})
}

func UserAgentFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v := ctx.Value(userAgentContextKey); v != nil {
		return v.(string)
	}
	return ""
}

func WithClusterScope(apiHandler http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var clusterName logicalcluster.Name
		if path := req.URL.Path; strings.HasPrefix(path, "/clusters/") {
			path = strings.TrimPrefix(path, "/clusters/")

			i := strings.Index(path, "/")
			if i == -1 {
				responsewriters.ErrorNegotiated(
					apierrors.NewBadRequest(fmt.Sprintf("unable to parse cluster: no `/` found in path %s", path)),
					errorCodecs, schema.GroupVersion{},
					w, req)
				return
			}
			clusterName, path = logicalcluster.New(path[:i]), path[i:]
			req.URL.Path = path
			for i := 0; i < 2 && len(req.URL.RawPath) > 1; i++ {
				slash := strings.Index(req.URL.RawPath[1:], "/")
				if slash == -1 {
					responsewriters.ErrorNegotiated(
						apierrors.NewInternalError(fmt.Errorf("unable to parse cluster when shortening raw path, have clusterName=%q, rawPath=%q", clusterName, req.URL.RawPath)),
						errorCodecs, schema.GroupVersion{},
						w, req)
					return
				}
				req.URL.RawPath = req.URL.RawPath[slash:]
			}
		} else {
			clusterName = logicalcluster.New(req.Header.Get(logicalcluster.ClusterHeader))
		}

		var cluster request.Cluster

		// This is necessary so wildcard (cross-cluster) partial metadata requests can succeed. The storage layer needs
		// to know if a request is for partial metadata to be able to extract the cluster name from storage keys
		// properly.
		cluster.PartialMetadataRequest = isPartialMetadataRequest(req.Context())

		switch {
		case clusterName == logicalcluster.Wildcard:
			// HACK: just a workaround for testing
			cluster.Wildcard = true
			// fallthrough
			cluster.Name = logicalcluster.Wildcard
		case clusterName.Empty():
			cluster.Name = genericcontrolplane.LocalAdminCluster
		default:
			if !reClusterName.MatchString(clusterName.String()) {
				responsewriters.ErrorNegotiated(
					apierrors.NewBadRequest(fmt.Sprintf("invalid cluster: %q does not match the regex", clusterName)),
					errorCodecs, schema.GroupVersion{},
					w, req)
				return
			}
			cluster.Name = clusterName
		}

		ctx := request.WithCluster(req.Context(), cluster)

		apiHandler.ServeHTTP(w, req.WithContext(ctx))
	}
}

// WithAuditAnnotation initializes audit annotations in the context. Without
// initialization kaudit.AddAuditAnnotation isn't preserved.
func WithAuditAnnotation(handler http.Handler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		handler.ServeHTTP(w, req.WithContext(
			kaudit.WithAuditAnnotations(req.Context()),
		))
	})
}

// WithClusterAnnotation adds the cluster name into the annotation of an audit
// event. Needs initialized annotations.
func WithClusterAnnotation(handler http.Handler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cluster := request.ClusterFrom(req.Context())
		if cluster != nil {
			kaudit.AddAuditAnnotation(req.Context(), workspaceAnnotation, cluster.Name.String())
		}

		handler.ServeHTTP(w, req)
	})
}

// WithWorkspaceProjection maps the personal virtual workspace "workspaces" resource into the cluster
// workspace URL space. This means you can do `kubectl get workspaces` from an org workspace.
func WithWorkspaceProjection(apiHandler http.Handler) http.HandlerFunc {
	toRedirectPath := path.Join("/apis", tenancyv1beta1.SchemeGroupVersion.Group, tenancyv1beta1.SchemeGroupVersion.Version, "workspaces/")
	getHomeWorkspaceRequestPath := path.Join(toRedirectPath, "~")

	return func(w http.ResponseWriter, req *http.Request) {
		cluster := request.ClusterFrom(req.Context())
		if cluster.Name.Empty() {
			apiHandler.ServeHTTP(w, req)
			return
		}

		if cluster.Name == tenancyv1alpha1.RootCluster && req.URL.Path == getHomeWorkspaceRequestPath {
			// Do not rewrite URL to point to the `workspaces` virtual workspace if we are in the special case
			// of a `kubectl get workspace ~` request which returns the Home workspace definition of the
			// current user.
			// This special request is managed later in the handler chain by the home workspace handler.
			apiHandler.ServeHTTP(w, req)
			return
		}

		if strings.HasPrefix(req.URL.Path, toRedirectPath) {
			newPath := path.Join("/services/workspaces", cluster.Name.String(), "all", req.URL.Path)
			klog.V(4).Infof("Rewriting %s -> %s", path.Join(cluster.Name.Path(), req.URL.Path), newPath)
			req.URL.Path = newPath
		}

		apiHandler.ServeHTTP(w, req)
	}
}

func WithWildcardListWatchGuard(apiHandler http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		cluster := request.ClusterFrom(req.Context())

		if cluster != nil && cluster.Wildcard {
			requestInfo, ok := request.RequestInfoFrom(req.Context())
			if !ok {
				responsewriters.ErrorNegotiated(
					apierrors.NewInternalError(fmt.Errorf("missing requestInfo")),
					errorCodecs, schema.GroupVersion{}, w, req,
				)

				return
			}

			if requestInfo.IsResourceRequest && !sets.NewString("list", "watch").Has(requestInfo.Verb) {
				statusErr := apierrors.NewMethodNotSupported(schema.GroupResource{Group: requestInfo.APIGroup, Resource: requestInfo.Resource}, requestInfo.Verb)
				statusErr.ErrStatus.Message += " in the `*` logical cluster"

				responsewriters.ErrorNegotiated(
					statusErr,
					errorCodecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, w, req,
				)

				return
			}
		}

		apiHandler.ServeHTTP(w, req)
	}
}

// WithInClusterServiceAccountRequestRewrite adds the /clusters/<clusterName> prefix to the request path if the request comes
// from an InCluster service account requests (InCluster clients don't support prefixes).
func WithInClusterServiceAccountRequestRewrite(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// some headers we set to set logical clusters, those are not the requests from InCluster clients
		clusterHeader := req.Header.Get(logicalcluster.ClusterHeader)
		shardedHeader := req.Header.Get("X-Kubernetes-Sharded-Request")

		if clusterHeader != "" || shardedHeader != "" {
			handler.ServeHTTP(w, req)
			return
		}

		if strings.HasPrefix(req.RequestURI, "/clusters/") {
			handler.ServeHTTP(w, req)
			return
		}

		prefix := "Bearer "
		token := req.Header.Get("Authorization")
		if !strings.HasPrefix(token, prefix) {
			handler.ServeHTTP(w, req)
			return
		}
		token = token[len(prefix):]

		var claims map[string]interface{}
		decoded, err := jwt2.ParseSigned(token)
		if err != nil { // just ignore
			handler.ServeHTTP(w, req)
			return
		}
		if err = decoded.UnsafeClaimsWithoutVerification(&claims); err != nil {
			handler.ServeHTTP(w, req)
			return
		}

		clusterName, ok, err := unstructured.NestedString(claims, "kubernetes.io", "clusterName") // bound
		if err != nil || !ok {
			clusterName, ok, err = unstructured.NestedString(claims, "kubernetes.io/serviceaccount/clusterName") // legacy
			if err != nil || !ok {
				handler.ServeHTTP(w, req)
				return
			}
		}

		req.URL.Path = path.Join("/clusters", clusterName, req.URL.Path)
		req.RequestURI = path.Join("/clusters", clusterName, req.RequestURI)

		handler.ServeHTTP(w, req)
	})
}

// WithWildcardIdentity checks wildcard list/watch requests for an APIExport identity for the resource in the path.
// If it finds one (e.g. /api/v1/services:identityabcd1234/default/my-service), it places the identity from the path
// to the context, updates the request to remove the identity from the path, and updates requestInfo.Resource to also
// remove the identity. Finally, it hands off to the passed in handler to handle the request.
func WithWildcardIdentity(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cluster := request.ClusterFrom(req.Context())
		if cluster == nil || !cluster.Wildcard {
			handler.ServeHTTP(w, req)
			return
		}

		requestInfo, ok := request.RequestInfoFrom(req.Context())
		if !ok {
			responsewriters.ErrorNegotiated(
				apierrors.NewInternalError(fmt.Errorf("missing requestInfo")),
				errorCodecs, schema.GroupVersion{}, w, req,
			)
			return
		}

		updatedReq, err := processResourceIdentity(req, requestInfo)
		if err != nil {
			klog.Errorf("WithWildcardIdentity: unable to determine resource from path %s", req.URL.Path)

			responsewriters.ErrorNegotiated(
				apierrors.NewInternalError(err),
				errorCodecs, schema.GroupVersion{}, w, req,
			)

			return
		}

		handler.ServeHTTP(w, updatedReq)
	})
}

func processResourceIdentity(req *http.Request, requestInfo *request.RequestInfo) (*http.Request, error) {
	if !requestInfo.IsResourceRequest {
		return req, nil
	}

	i := strings.Index(requestInfo.Resource, ":")

	if i < 0 {
		return req, nil
	}

	resource := requestInfo.Resource[:i]
	identity := requestInfo.Resource[i+1:]

	if identity == "" {
		return nil, fmt.Errorf("invalid resource %q: missing identity", resource)
	}

	req = utilnet.CloneRequest(req)

	req = req.WithContext(WithIdentity(req.Context(), identity))

	req.URL.Path = strings.Replace(req.URL.Path, requestInfo.Resource, resource, 1)
	req.URL.RawPath = strings.Replace(req.URL.RawPath, requestInfo.Resource, resource, 1)

	requestInfo.Resource = resource

	return req, nil
}

func mergeCRDsIntoCoreGroup(crdLister *apiBindingAwareCRDLister, crdHandler, coreHandler func(res http.ResponseWriter, req *http.Request)) restful.FilterFunction {
	return func(req *restful.Request, res *restful.Response, chain *restful.FilterChain) {
		ctx := req.Request.Context()
		requestInfo, ok := request.RequestInfoFrom(ctx)
		if !ok {
			responsewriters.ErrorNegotiated(
				apierrors.NewInternalError(fmt.Errorf("no RequestInfo found in the context")),
				// TODO is this the right Codecs?
				errorCodecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, res.ResponseWriter, req.Request,
			)
			return
		}

		// If it's not the core group, pass through
		if requestInfo.APIGroup != "" {
			chain.ProcessFilter(req, res)
			return
		}

		//
		// from here on we know it's the core group
		//

		if !requestInfo.IsResourceRequest && (req.Request.URL.Path == "/api/v1" || req.Request.URL.Path == "/api/v1/") {
			// This is a discovery request. We may need to combine discovery from the GenericControlPlane (which has built-in v1 types)
			// and CRDs, if there are any v1 CRDs.

			// Because of the way the http handlers are configured for /api/v1, we have to do something a bit unique to make this work.
			// /api/v1 is ultimately served by the GoRestfulContainer. This means we have to put a filter on it to be able to change the
			// behavior. And because the filter runs for the client's initial /api/v1 request, and when we pass the request down to
			// the generic control plane to get its discovery for /api/v1, we have to do something to short circuit our filter to
			// avoid infinite recursion. This is done below using passthroughHeader.
			//
			// The initial request, from a client, won't have this header set. We set it and send the /api/v1 request to the generic control
			// plane. This re-invokes this filter, but because the header is set, we pass the request through to the rest of the filter chain,
			// meaning it will be sent to the generic control plane to return its /api/v1 discovery.

			// If we are retrieving the GenericControlPlane's v1 APIResources, pass it through to the filter chain.
			if _, passthrough := req.Request.Header[passthroughHeader]; passthrough {
				chain.ProcessFilter(req, res)
				return
			}

			// If we're here, it means it's an initial /api/v1 request from a client.

			serveCoreV1Discovery(ctx, crdLister, coreHandler, res.ResponseWriter, req.Request)
			return
		}

		if requestInfo.IsResourceRequest {
			// This is a CRUD request for something like pods. Try to see if there is a CRD for the resource. If so, let the CRD
			// server handle it.
			crdName := requestInfo.Resource + ".core"

			if _, err := crdLister.Get(req.Request.Context(), crdName); err == nil {
				crdHandler(res.ResponseWriter, req.Request)
				return
			}

			// fall-through to the native types
		}

		// Fall back to pass through if we didn't match anything above
		chain.ProcessFilter(req, res)
	}
}

func serveCoreV1Discovery(ctx context.Context, crdLister *apiBindingAwareCRDLister, coreHandler func(w http.ResponseWriter, req *http.Request), res http.ResponseWriter, req *http.Request) {
	// Get all the CRDs to see if any of them are in v1
	crds, err := crdLister.List(ctx, labels.Everything())
	if err != nil {
		// Listing from a lister can really only ever fail if invoking meta.Accesor() on an item in the list fails.
		// Which means it essentially will never fail. But just in case...
		err = apierrors.NewInternalError(fmt.Errorf("unable to serve /api/v1 discovery: error listing CustomResourceDefinitions: %w", err))
		_ = responsewriters.ErrorNegotiated(err, errorCodecs, schema.GroupVersion{}, res, req)
		return
	}

	// Generate discovery for the CRDs.
	crdDiscovery := apiextensionsapiserver.APIResourcesForGroupVersion("", "v1", crds)

	// v1 CRDs present - need to clone the request, add our passthrough header, and get /api/v1 discovery from
	// the GenericControlPlane's server.
	cr := utilnet.CloneRequest(req)
	cr.Header.Add(passthroughHeader, "1")

	writer := newInMemoryResponseWriter()
	coreHandler(writer, cr)
	if writer.respCode != http.StatusOK {
		// Write the response back to the client
		res.WriteHeader(writer.respCode)
		res.Write(writer.data) //nolint:errcheck
		return
	}

	// Decode the response. Have to pass into correctly (instead of nil) because APIResourceList
	// is "special" - it doesn't have an apiVersion field that the decoder needs to determine the
	// type.
	into := &metav1.APIResourceList{}
	obj, _, err := aggregator.DiscoveryCodecs.UniversalDeserializer().Decode(writer.data, nil, into)
	if err != nil {
		err = apierrors.NewInternalError(fmt.Errorf("unable to serve /api/v1 discovery: error decoding /api/v1 response from generic control plane: %w", err))
		_ = responsewriters.ErrorNegotiated(err, errorCodecs, schema.GroupVersion{}, res, req)
		return
	}
	v1ResourceList, ok := obj.(*metav1.APIResourceList)
	if !ok {
		err = apierrors.NewInternalError(fmt.Errorf("unable to serve /api/v1 discovery: error decoding /api/v1 response from generic control plane: unexpected data type %T", obj))
		_ = responsewriters.ErrorNegotiated(err, errorCodecs, schema.GroupVersion{}, res, req)
		return
	}

	// Combine the 2 sets of discovery resources
	v1ResourceList.APIResources = append(v1ResourceList.APIResources, crdDiscovery...)

	// Sort based on resource name
	sort.SliceStable(v1ResourceList.APIResources, func(i, j int) bool {
		return v1ResourceList.APIResources[i].Name < v1ResourceList.APIResources[j].Name
	})

	// Serve up our combined discovery
	versionHandler := apiserverdiscovery.NewAPIVersionHandler(aggregator.DiscoveryCodecs, schema.GroupVersion{Group: "", Version: "v1"}, apiserverdiscovery.APIResourceListerFunc(func() []metav1.APIResource {
		return v1ResourceList.APIResources
	}))
	versionHandler.ServeHTTP(res, req)
}

// COPIED FROM kube-aggregator
// inMemoryResponseWriter is a http.Writer that keep the response in memory.
type inMemoryResponseWriter struct {
	writeHeaderCalled bool
	header            http.Header
	respCode          int
	data              []byte
}

func newInMemoryResponseWriter() *inMemoryResponseWriter {
	return &inMemoryResponseWriter{header: http.Header{}}
}

func (r *inMemoryResponseWriter) Header() http.Header {
	return r.header
}

func (r *inMemoryResponseWriter) WriteHeader(code int) {
	r.writeHeaderCalled = true
	r.respCode = code
}

func (r *inMemoryResponseWriter) Write(in []byte) (int, error) {
	if !r.writeHeaderCalled {
		r.WriteHeader(http.StatusOK)
	}
	r.data = append(r.data, in...)
	return len(in), nil
}

func (r *inMemoryResponseWriter) String() string {
	s := fmt.Sprintf("ResponseCode: %d", r.respCode)
	if r.data != nil {
		s += fmt.Sprintf(", Body: %s", string(r.data))
	}
	if r.header != nil {
		s += fmt.Sprintf(", Header: %s", r.header)
	}
	return s
}

// unimplementedServiceResolver is a webhook.ServiceResolver that always returns an error, because
// we have not implemented support for this yet. As a result, CRD webhook conversions are not
// supported.
type unimplementedServiceResolver struct{}

// ResolveEndpoint always returns an error that this is not yet supported.
func (r *unimplementedServiceResolver) ResolveEndpoint(namespace string, name string, port int32) (*url.URL, error) {
	return nil, errors.New("CRD webhook conversions are not yet supported in kcp")
}
