/*
Copyright 2022 The KCP Authors.

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

package clusterworkspace

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/kcp-dev/logicalcluster"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"github.com/kcp-dev/kcp/pkg/admission/helpers"
	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
)

func createAttr(ws *tenancyv1alpha1.ClusterWorkspace) admission.Attributes {
	return createAttrWithUser(ws, &user.DefaultInfo{})
}

func createAttrWithUser(ws *tenancyv1alpha1.ClusterWorkspace, info user.Info) admission.Attributes {
	return admission.NewAttributesRecord(
		helpers.ToUnstructuredOrDie(ws),
		nil,
		tenancyv1alpha1.Kind("ClusterWorkspace").WithVersion("v1alpha1"),
		"",
		ws.Name,
		tenancyv1alpha1.Resource("clusterworkspaces").WithVersion("v1alpha1"),
		"",
		admission.Create,
		&metav1.CreateOptions{},
		false,
		info,
	)
}

func updateAttr(ws, old *tenancyv1alpha1.ClusterWorkspace) admission.Attributes {
	return admission.NewAttributesRecord(
		helpers.ToUnstructuredOrDie(ws),
		helpers.ToUnstructuredOrDie(old),
		tenancyv1alpha1.Kind("ClusterWorkspace").WithVersion("v1alpha1"),
		"",
		ws.Name,
		tenancyv1alpha1.Resource("clusterworkspaces").WithVersion("v1alpha1"),
		"",
		admission.Update,
		&metav1.CreateOptions{},
		false,
		&user.DefaultInfo{},
	)
}

func TestAdmit(t *testing.T) {
	tests := []struct {
		name        string
		types       []*tenancyv1alpha1.ClusterWorkspaceType
		workspaces  []*tenancyv1alpha1.ClusterWorkspace
		clusterName logicalcluster.Name
		a           admission.Attributes
		expectedObj runtime.Object
		wantErr     bool
	}{
		{
			name: "adds user information on create",
			types: []*tenancyv1alpha1.ClusterWorkspaceType{
				newType("root:org:foo").ClusterWorkspaceType,
			},
			clusterName: logicalcluster.New("root:org:ws"),
			a: createAttrWithUser(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
			}, &user.DefaultInfo{
				Name:   "someone",
				UID:    "id",
				Groups: []string{"a", "b"},
				Extra: map[string][]string{
					"one": {"1", "01"},
				},
			}),
			expectedObj: &tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
					Annotations: map[string]string{
						"tenancy.kcp.dev/owner": `{"username":"someone","uid":"id","groups":["a","b"],"extra":{"one":["1","01"]}}`,
					},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &clusterWorkspace{
				Handler: admission.NewHandler(admission.Create, admission.Update),
			}
			ctx := request.WithCluster(context.Background(), request.Cluster{Name: tt.clusterName})
			if err := o.Admit(ctx, tt.a, nil); (err != nil) != tt.wantErr {
				t.Fatalf("Admit() error = %v, wantErr %v", err, tt.wantErr)
			} else if err == nil {
				got, ok := tt.a.GetObject().(*unstructured.Unstructured)
				require.True(t, ok, "expected unstructured, got %T", tt.a.GetObject())
				expected := helpers.ToUnstructuredOrDie(tt.expectedObj)
				if diff := cmp.Diff(expected, got); diff != "" {
					t.Fatalf("got incorrect result: %v", diff)
				}
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name           string
		a              admission.Attributes
		expectedErrors []string
	}{
		{
			name: "rejects type mutations",
			a: updateAttr(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
			},
				&tenancyv1alpha1.ClusterWorkspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test",
						Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
					},
					Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
						Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
							Name: "universal",
							Path: "root:org",
						},
					},
				}),
			expectedErrors: []string{"field is immutable"},
		},
		{
			name: "rejects unsetting location",
			a: updateAttr(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
				Status: tenancyv1alpha1.ClusterWorkspaceStatus{
					Location: tenancyv1alpha1.ClusterWorkspaceLocation{
						Current: "",
					},
				}},
				&tenancyv1alpha1.ClusterWorkspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test",
						Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
					},
					Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
						Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
							Name: "foo",
							Path: "root:org",
						},
					},
					Status: tenancyv1alpha1.ClusterWorkspaceStatus{
						Location: tenancyv1alpha1.ClusterWorkspaceLocation{
							Current: "cluster",
						},
					},
				}),
			expectedErrors: []string{"status.location.current cannot be unset"},
		},
		{
			name: "rejects unsetting baseURL",
			a: updateAttr(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
				Status: tenancyv1alpha1.ClusterWorkspaceStatus{},
			},
				&tenancyv1alpha1.ClusterWorkspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test",
						Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
					},
					Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
						Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
							Name: "foo",
							Path: "root:org",
						},
					},
					Status: tenancyv1alpha1.ClusterWorkspaceStatus{
						BaseURL: "https://cluster/clsuters/test",
					},
				}),
			expectedErrors: []string{"status.baseURL cannot be unset"},
		},
		{
			name: "rejects transition from Initializing with non-empty initializers",
			a: updateAttr(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
				Status: tenancyv1alpha1.ClusterWorkspaceStatus{
					Phase:        tenancyv1alpha1.ClusterWorkspacePhaseReady,
					Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{"a"},
					Location:     tenancyv1alpha1.ClusterWorkspaceLocation{Current: "somewhere"},
					BaseURL:      "https://kcp.bigcorp.com/clusters/org:test",
				},
			},
				&tenancyv1alpha1.ClusterWorkspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test",
						Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
					},
					Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
						Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
							Name: "foo",
							Path: "root:org",
						},
					},
					Status: tenancyv1alpha1.ClusterWorkspaceStatus{
						Phase:        tenancyv1alpha1.ClusterWorkspacePhaseInitializing,
						Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{"a"},
					},
				}),
			expectedErrors: []string{"spec.initializers must be empty for phase Ready"},
		},
		{
			name: "allows transition from Initializing with empty initializers",
			a: updateAttr(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
				Status: tenancyv1alpha1.ClusterWorkspaceStatus{
					Phase:        tenancyv1alpha1.ClusterWorkspacePhaseReady,
					Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{},
					Location:     tenancyv1alpha1.ClusterWorkspaceLocation{Current: "somewhere"},
					BaseURL:      "https://kcp.bigcorp.com/clusters/org:test",
				},
			},
				&tenancyv1alpha1.ClusterWorkspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test",
						Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
					},
					Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
						Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
							Name: "foo",
							Path: "root:org",
						},
					},
					Status: tenancyv1alpha1.ClusterWorkspaceStatus{
						Phase:        tenancyv1alpha1.ClusterWorkspacePhaseInitializing,
						Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{"a"},
						Location:     tenancyv1alpha1.ClusterWorkspaceLocation{Current: "somewhere"},
						BaseURL:      "https://kcp.bigcorp.com/clusters/org:test",
					},
				}),
		},
		{
			name: "allows transition to ready directly when valid",
			a: updateAttr(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
				Status: tenancyv1alpha1.ClusterWorkspaceStatus{
					Phase:        tenancyv1alpha1.ClusterWorkspacePhaseReady,
					Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{},
					Location:     tenancyv1alpha1.ClusterWorkspaceLocation{Current: "somewhere"},
					BaseURL:      "https://kcp.bigcorp.com/clusters/org:test",
				},
			},
				&tenancyv1alpha1.ClusterWorkspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test",
						Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
					},
					Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
						Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
							Name: "foo",
							Path: "root:org",
						},
					},
					Status: tenancyv1alpha1.ClusterWorkspaceStatus{
						Phase:        tenancyv1alpha1.ClusterWorkspacePhaseScheduling,
						Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{"a"},
					},
				}),
		},
		{
			name: "allows creation to ready directly when valid",
			a: createAttr(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
				Status: tenancyv1alpha1.ClusterWorkspaceStatus{
					Phase:        tenancyv1alpha1.ClusterWorkspacePhaseReady,
					Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{},
					Location:     tenancyv1alpha1.ClusterWorkspaceLocation{Current: "somewhere"},
					BaseURL:      "https://kcp.bigcorp.com/clusters/org:test",
				},
			}),
		},
		{
			name: "rejects transition to ready directly when invalid",
			a: updateAttr(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
				Status: tenancyv1alpha1.ClusterWorkspaceStatus{
					Phase:        tenancyv1alpha1.ClusterWorkspacePhaseReady,
					Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{},
					Location:     tenancyv1alpha1.ClusterWorkspaceLocation{Current: "somewhere"},
				},
			},
				&tenancyv1alpha1.ClusterWorkspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test",
						Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
					},
					Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
						Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
							Name: "foo",
							Path: "root:org",
						},
					},
					Status: tenancyv1alpha1.ClusterWorkspaceStatus{
						Phase:        tenancyv1alpha1.ClusterWorkspacePhaseScheduling,
						Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{"a"},
					},
				}),
			expectedErrors: []string{"status.baseURL must be set for phase Ready"},
		},
		{
			name: "rejects transition to previous phase",
			a: updateAttr(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
				Status: tenancyv1alpha1.ClusterWorkspaceStatus{
					Phase:        tenancyv1alpha1.ClusterWorkspacePhaseInitializing,
					Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{},
					Location:     tenancyv1alpha1.ClusterWorkspaceLocation{Current: "somewhere"},
					BaseURL:      "https://kcp.bigcorp.com/clusters/org:test",
				},
			},
				&tenancyv1alpha1.ClusterWorkspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test",
						Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
					},
					Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
						Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
							Name: "foo",
							Path: "root:org",
						},
					},
					Status: tenancyv1alpha1.ClusterWorkspaceStatus{
						Phase:        tenancyv1alpha1.ClusterWorkspacePhaseReady,
						Initializers: []tenancyv1alpha1.ClusterWorkspaceInitializer{},
						Location:     tenancyv1alpha1.ClusterWorkspaceLocation{Current: "somewhere"},
						BaseURL:      "https://kcp.bigcorp.com/clusters/org:test",
					},
				}),
			expectedErrors: []string{"cannot transition from \"Ready\" to \"Initializing\""},
		},
		{
			name: "ignores different resources",
			a: admission.NewAttributesRecord(
				&tenancyv1alpha1.ClusterWorkspaceShard{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test",
						Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
					},
				},
				nil,
				tenancyv1alpha1.Kind("ClusterWorkspaceShard").WithVersion("v1alpha1"),
				"",
				"test",
				tenancyv1alpha1.Resource("clusterworkspaceshards").WithVersion("v1alpha1"),
				"",
				admission.Create,
				&metav1.CreateOptions{},
				false,
				&user.DefaultInfo{},
			),
		},
		{
			name: "checks user information on create",
			a: createAttrWithUser(&tenancyv1alpha1.ClusterWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: map[string]string{"tenancy.kcp.dev/owner": "{}"},
				},
				Spec: tenancyv1alpha1.ClusterWorkspaceSpec{
					Type: tenancyv1alpha1.ClusterWorkspaceTypeReference{
						Name: "foo",
						Path: "root:org",
					},
				},
			}, &user.DefaultInfo{
				Name:   "someone",
				UID:    "id",
				Groups: []string{"a", "b"},
				Extra: map[string][]string{
					"one": {"1", "01"},
				},
			}),
			expectedErrors: []string{"expected user annotation tenancy.kcp.dev/owner={\"username\":\"someone\",\"uid\":\"id\",\"groups\":[\"a\",\"b\"],\"extra\":{\"one\":[\"1\",\"01\"]}}"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &clusterWorkspace{
				Handler: admission.NewHandler(admission.Create, admission.Update),
			}
			ctx := request.WithCluster(context.Background(), request.Cluster{Name: logicalcluster.New("root:org")})
			err := o.Validate(ctx, tt.a, nil)
			t.Logf("%v", err)
			wantErr := len(tt.expectedErrors) > 0
			require.Equal(t, wantErr, err != nil)

			if err != nil {
				t.Logf("Got admission errors: %v", err)
				for _, expected := range tt.expectedErrors {
					require.Contains(t, err.Error(), expected)
				}
			}
		})
	}
}

type builder struct {
	*tenancyv1alpha1.ClusterWorkspaceType
}

func newType(qualifiedName string) builder {
	path, name := logicalcluster.New(qualifiedName).Split()
	return builder{ClusterWorkspaceType: &tenancyv1alpha1.ClusterWorkspaceType{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			ClusterName: path.String(),
		},
	}}
}
