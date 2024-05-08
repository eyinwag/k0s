/*
Copyright 2024 k0s authors

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

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	kubeutil "github.com/k0sproject/k0s/pkg/kubernetes"
	"github.com/k0sproject/k0s/pkg/kubernetes/watch"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// CPLBReconciler monitors the endpoints of the "kubernetes" service in the
// "default" namespace. It notifies changes though the updateCh channel provided
// in the constructor.
type CPLBReconciler struct {
	log            *logrus.Entry
	kubeconfigPath string
	addresses      []string
	mu             sync.RWMutex
	updateCh       chan<- struct{}
	stop           func()
}

func NewCPLBReconciler(kubeconfigPath string, updateCh chan<- struct{}) *CPLBReconciler {
	return &CPLBReconciler{
		log:            logrus.WithField("component", "cplb-reconciler"),
		kubeconfigPath: kubeconfigPath,
		updateCh:       updateCh,
	}
}

func (r *CPLBReconciler) Start() error {
	clientset, err := kubeutil.NewClientFromFile(r.kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to get clientset: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		r.watchAPIServers(ctx, clientset)
	}()

	r.stop = func() { cancel(); <-done }

	return nil
}

func (r *CPLBReconciler) Stop() {
	r.log.Debug("Stopping")
	r.stop()
	r.log.Info("Stopped")
}

func (r *CPLBReconciler) watchAPIServers(ctx context.Context, clientSet kubernetes.Interface) {
	for {
		select {
		default:
			err := watch.Endpoints(clientSet.CoreV1().Endpoints("default")).
				WithObjectName("kubernetes").
				Until(ctx, func(endpoints *corev1.Endpoints) (bool, error) {
					r.maybeUpdateIPs(endpoints)
					return false, nil
				})
			// Log any reconciliation errors, but only if they don't
			// indicate that the reconciler has been stopped.
			if err != nil && !errors.Is(err, ctx.Err()) {
				r.log.WithError(err).Error("Failed to reconcile API server addresses")
			}

			// After a watch error wait 5 seconds before retrying
			time.Sleep(5 * time.Second)

		case <-ctx.Done():
			r.log.Info("Stopped watching kubernetes endpoints")
			return
		}
	}
}

// maybeUpdateIPs updates the list of IP addresses if the new list has
// different addresses
func (r *CPLBReconciler) maybeUpdateIPs(endpoint *corev1.Endpoints) {
	newAddresses := []string{}
	for _, subset := range endpoint.Subsets {
		for _, addr := range subset.Addresses {
			newAddresses = append(newAddresses, addr.IP)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// endpoints are not guaranteed to be sorted by IP address
	slices.Sort(newAddresses)

	if !slices.Equal(r.addresses, newAddresses) {
		r.addresses = newAddresses
		r.log.Infof("Updated the list of IPs: %v", r.addresses)
		select {
		case r.updateCh <- struct{}{}:
		default:
		}
	}
}

// GetIPs gets a thread-safe copy of the current list of IP addresses
func (r *CPLBReconciler) GetIPs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.addresses)
}
