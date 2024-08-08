/*
Copyright The Ratify Authors.
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

package clusterresource

import (
	"context"
	"errors"
	"testing"

	"github.com/ratify-project/ratify/pkg/keymanagementprovider/refresh"
	test "github.com/ratify-project/ratify/pkg/utils"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKeyManagementProviderReconciler_Reconcile(t *testing.T) {
	tests := []struct {
		name          string
		refresherType string
		expectedError bool
	}{
		{
			name:          "Successful Reconcile",
			refresherType: "mockRefresher",
			expectedError: false,
		},
		{
			name:          "Refresher Error",
			refresherType: "mockRefresher",
			expectedError: true,
		},
		{
			name:          "Invalid Refresher Type",
			refresherType: "invalidRefresher",
			expectedError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ctrl.Request{
				NamespacedName: client.ObjectKey{
					Name:      "fake-name",
					Namespace: "fake-namespace",
				},
			}
			scheme, _ := test.CreateScheme()
			client := fake.NewClientBuilder().WithScheme(scheme).Build()

			r := &KeyManagementProviderReconciler{
				Client: client,
				Scheme: runtime.NewScheme(),
			}

			refresherConfig := map[string]interface{}{
				"type":        tt.refresherType,
				"client":      client,
				"request":     req,
				"shouldError": tt.expectedError,
			}

			_, err := r.ReconcileWithConfig(context.TODO(), refresherConfig)
			if tt.expectedError && err == nil {
				t.Errorf("Expected error, got nil")
			}
			if !tt.expectedError && err != nil {
				t.Errorf("Expected no error, got %v", err)
			}
		})
	}
}

type MockRefresher struct {
	Results     ctrl.Result
	ShouldError bool
}

func (mr *MockRefresher) Refresh(ctx context.Context) error {
	if mr.ShouldError {
		return errors.New("refresh error")
	}
	return nil
}

func (mr *MockRefresher) GetResult() interface{} {
	return ctrl.Result{}
}

func (mr *MockRefresher) Create(config map[string]interface{}) (refresh.Refresher, error) {
	shouldError := config["shouldError"].(bool)
	if shouldError {
		return nil, errors.New("create error")
	}
	return &MockRefresher{
		ShouldError: shouldError,
	}, nil
}

func init() {
	refresh.Register("mockRefresher", &MockRefresher{})
}
