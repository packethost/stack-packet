/*
Copyright 2019 The Crossplane Authors.

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

package device

import (
	"context"
	"fmt"

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha2 "github.com/packethost/crossplane-provider-equinix-metal/apis/server/v1alpha2"
	packetv1beta1 "github.com/packethost/crossplane-provider-equinix-metal/apis/v1beta1"
	"github.com/packethost/crossplane-provider-equinix-metal/pkg/clients"
	packetclient "github.com/packethost/crossplane-provider-equinix-metal/pkg/clients"
	devicesclient "github.com/packethost/crossplane-provider-equinix-metal/pkg/clients/device"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
)

// Error strings.
const (
	errManagedUpdateFailed     = "cannot update Device custom resource"
	errTrackPCUsage            = "cannot track ProviderConfig usage"
	errGetProviderConfigSecret = "cannot get ProviderConfig Secret"
	errGenObservation          = "cannot generate observation"
	errNewClient               = "cannot create new Device client"
	errNotDevice               = "managed resource is not a Device"
	errGetDevice               = "cannot get Device"
	errCreateDevice            = "cannot create Device"
	errUpdateDevice            = "cannot modify Device"
	errDeleteDevice            = "cannot delete Device"

	userdataMapKey = "cloud-init"
)

// SetupDevice adds a controller that reconciles Devices
func SetupDevice(mgr ctrl.Manager, l logging.Logger) error {
	name := managed.ControllerName(v1alpha2.DeviceGroupKind)

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha2.DeviceGroupVersionKind),
		managed.WithExternalConnecter(&connecter{
			kube:  mgr.GetClient(),
			usage: resource.NewProviderConfigUsageTracker(mgr.GetClient(), &packetv1beta1.ProviderConfigUsage{}),
		}),
		managed.WithLogger(l.WithValues("controller", name)),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&v1alpha2.Device{}).
		Complete(r)
}

type connecter struct {
	kube        client.Client
	usage       resource.Tracker
	newClientFn func(ctx context.Context, config *clients.Credentials) (devicesclient.ClientWithDefaults, error)
}

func (c *connecter) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	if _, ok := mg.(*v1alpha2.Device); !ok {
		return nil, errors.New(errNotDevice)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	newClientFn := devicesclient.NewClient
	if c.newClientFn != nil {
		newClientFn = c.newClientFn
	}

	cfg, err := clients.GetAuthInfo(ctx, c.kube, mg)
	if err != nil {
		return nil, errors.Wrap(err, errGetProviderConfigSecret)
	}
	client, err := newClientFn(ctx, cfg)

	return &external{kube: c.kube, client: client}, errors.Wrap(err, errNewClient)
}

type external struct {
	kube   client.Client
	client devicesclient.ClientWithDefaults
}

func (e *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) { //nolint:gocyclo
	d, ok := mg.(*v1alpha2.Device)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotDevice)
	}

	// Observe device
	device, _, err := e.client.Get(meta.GetExternalName(d), nil)
	if packetclient.IsNotFound(err) {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, errGetDevice)
	}

	current := d.Spec.ForProvider.DeepCopy()
	devicesclient.LateInitialize(&d.Spec.ForProvider, device)
	if !cmp.Equal(current, &d.Spec.ForProvider) {
		if err := e.kube.Update(ctx, d); err != nil {
			return managed.ExternalObservation{}, errors.Wrap(err, errManagedUpdateFailed)
		}
	}

	d.Status.AtProvider, err = devicesclient.GenerateObservation(device)
	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, errGenObservation)
	}

	// Set Device status and bindable
	switch d.Status.AtProvider.State {
	case v1alpha2.StateActive:
		d.Status.SetConditions(xpv1.Available())
	case v1alpha2.StateProvisioning:
		d.Status.SetConditions(xpv1.Creating())
	case v1alpha2.StateQueued,
		v1alpha2.StateDeprovisioning,
		v1alpha2.StateFailed,
		v1alpha2.StateInactive,
		v1alpha2.StatePoweringOff,
		v1alpha2.StateReinstalling:
		d.Status.SetConditions(xpv1.Unavailable())
	}

	upToDate, networkTypeUpToDate := devicesclient.IsUpToDate(d, device)

	o := managed.ExternalObservation{
		ResourceExists:    true,
		ResourceUpToDate:  upToDate && networkTypeUpToDate,
		ConnectionDetails: devicesclient.GetConnectionDetails(device),
	}

	return o, nil
}

// resolveUserDataRefs returns a userdata string fetched from the referenced userdata resource
// TODO(displague) use reference.NewAPIResolver when TypedReference is support
func (e *external) resolveUserDataRefs(ctx context.Context, d *v1alpha2.Device) (string, error) { //nolint:gocyclo
	errGetUserDataRef := "cannot get required resource for UserDataRef"
	errInvalidRefKind := "invalid resource kind"
	errRefKeyNotFoundFmt := "could not find UserDataRef key %q"

	ref := d.Spec.ForProvider.UserDataRef
	var userdata string
	var ok bool
	nsn := types.NamespacedName{
		Name:      ref.Name,
		Namespace: ref.Namespace,
	}
	key := ref.Key
	if key == "" {
		key = userdataMapKey
	}

	switch ref.Kind {
	case "ConfigMap":
		resource := &corev1.ConfigMap{}
		err := e.kube.Get(ctx, nsn, resource)
		if err != nil && !ref.Optional {
			return "", errors.Wrap(err, errGetUserDataRef)
		}

		userdata, ok = resource.Data[key]
	case "Secret":
		resource := &corev1.Secret{}
		err := e.kube.Get(ctx, nsn, resource)
		if err != nil && !ref.Optional {
			return "", errors.Wrap(err, errGetUserDataRef)
		}
		var bytes []byte
		bytes, ok = resource.Data[key]
		userdata = string(bytes)
	default:
		return "", errors.Wrap(errors.New(errGetUserDataRef), errInvalidRefKind)
	}

	if !ok && !ref.Optional {
		err := errors.Wrap(fmt.Errorf(errGetUserDataRef), fmt.Sprintf(errRefKeyNotFoundFmt, key))
		return "", err
	}
	return userdata, nil
}

func (e *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	d, ok := mg.(*v1alpha2.Device)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotDevice)
	}

	d.Status.SetConditions(xpv1.Creating())

	createDev := d.DeepCopy()

	if d.Spec.ForProvider.UserDataRef != nil {
		userdata, err := e.resolveUserDataRefs(ctx, d)
		if err != nil {
			return managed.ExternalCreation{}, err
		}
		createDev.Spec.ForProvider.UserData = &userdata
	}

	create := devicesclient.CreateFromDevice(createDev, e.client.GetProjectID(packetclient.CredentialProjectID))
	device, _, err := e.client.Create(create)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errCreateDevice)
	}

	d.Status.AtProvider.ID = device.ID
	meta.SetExternalName(d, device.ID)
	if err := e.kube.Update(ctx, d); err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errManagedUpdateFailed)
	}

	return managed.ExternalCreation{ConnectionDetails: devicesclient.GetConnectionDetails(device)}, nil
}

func (e *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	d, ok := mg.(*v1alpha2.Device)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotDevice)
	}

	// NOTE(hasheddan): we must get the device again to see what type of update
	// we need to make
	device, _, err := e.client.Get(meta.GetExternalName(d), nil)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, errGetDevice)
	}

	// NOTE(hasheddan): if the update is for the network type we return early
	// and do any updates on subsequent reconciles
	if _, n := devicesclient.IsUpToDate(d, device); !n && d.Spec.ForProvider.NetworkType != nil {
		_, err := e.client.DeviceToNetworkType(meta.GetExternalName(d), *d.Spec.ForProvider.NetworkType)
		return managed.ExternalUpdate{}, errors.Wrap(err, errUpdateDevice)
	}
	_, _, err = e.client.Update(meta.GetExternalName(d), devicesclient.NewUpdateDeviceRequest(d))

	// TODO(displague): use "reinstall" action if userdata changed, after updating the resource

	return managed.ExternalUpdate{}, errors.Wrap(err, errUpdateDevice)
}

func (e *external) Delete(ctx context.Context, mg resource.Managed) error {
	d, ok := mg.(*v1alpha2.Device)
	if !ok {
		return errors.New(errNotDevice)
	}
	d.SetConditions(xpv1.Deleting())

	_, err := e.client.Delete(meta.GetExternalName(d), false)
	return errors.Wrap(resource.Ignore(packetclient.IsNotFound, err), errDeleteDevice)
}
