/*
Copyright 2024.

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
	"crypto/rand"
	"encoding/base64"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gatewayapi "sigs.k8s.io/gateway-api/apis/v1"
	v1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/cloudflare/cloudflare-go"
)

// GatewayReconciler reconciles a Gateway object
type GatewayReconciler struct {
	client.Client
	CloudflareClient    *cloudflare.API
	CloudflareAccountId string
	Scheme              *runtime.Scheme
}

const finalizer = "cloudflare-tunnel.pokgak.xyz/finalizer"

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Gateway object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.0/pkg/reconcile
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling Gateway")

	gateway := &v1.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, gateway); err != nil {
		logger.Error(err, "unable to fetch Gateway")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if gateway.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// to registering our finalizer.
		if !controllerutil.ContainsFinalizer(gateway, finalizer) {
			controllerutil.AddFinalizer(gateway, finalizer)
			if err := r.Update(ctx, gateway); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(gateway, finalizer) {
			// our finalizer is present, so lets handle any external dependency
			if err := r.deleteTunnel(gateway); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried.
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(gateway, finalizer)
			if err := r.Update(ctx, gateway); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// check if the tunnel is already created
	if gateway.Annotations["cloudflare-tunnel.pokgak.xyz/tunnel-id"] == "" {
		// create tunnel

		// Generate 32 byte random string for tunnel secret
		randSecret := make([]byte, 32)
		if _, err := rand.Read(randSecret); err != nil {
			return ctrl.Result{}, err
		}
		tunnelSecret := base64.StdEncoding.EncodeToString(randSecret)

		params := cloudflare.TunnelCreateParams{
			Name:   gateway.Name,
			Secret: tunnelSecret,
			// we're using remote tunnel
			ConfigSrc: "cloudflare",
		}

		rc := cloudflare.AccountIdentifier(r.CloudflareAccountId)
		tunnel, err := r.CloudflareClient.CreateTunnel(context.Background(), rc, params)
		if err != nil {
			return ctrl.Result{}, err
		}

		gateway.Annotations["cloudflare-tunnel.pokgak.xyz/tunnel-id"] = tunnel.ID
		gateway.Annotations["cloudflare-tunnel.pokgak.xyz/tunnel-secret"] = tunnelSecret
		if err := r.Update(ctx, gateway); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		// update tunnel
		rc := cloudflare.AccountIdentifier(r.CloudflareAccountId)
		tunnelId := gateway.Annotations["cloudflare-tunnel.pokgak.xyz/tunnel-id"]
		_, err := r.CloudflareClient.GetTunnel(context.Background(), rc, tunnelId)
		if err != nil {
			return ctrl.Result{}, err
		}

		// all good nothing to do here
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayapi.Gateway{}).
		Complete(r)
}

func (r *GatewayReconciler) deleteTunnel(gateway *v1.Gateway) error {
	rc := cloudflare.AccountIdentifier(r.CloudflareAccountId)
	tunnelId := gateway.Annotations["cloudflare-tunnel.pokgak.xyz/tunnel-id"]

	err := r.CloudflareClient.DeleteTunnel(context.Background(), rc, tunnelId)
	if err != nil {
		return err
	}

	return nil
}
