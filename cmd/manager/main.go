// Command manager is the entrypoint for the Amenotejikara controller: it
// wires the CredentialRotation reconciler into a controller-runtime
// Manager and starts it.
package main

import (
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	opsv1alpha1 "github.com/mykyta-kravchenko98/Amenotejikara/api/v1alpha1"
	"github.com/mykyta-kravchenko98/Amenotejikara/internal/controller"
)

// scheme is the registry controller-runtime uses to know how to
// encode/decode every Kind this manager will touch - the built-in ones
// (Secret, Deployment, ...) via clientgoscheme, plus our own CRD via
// opsv1alpha1.AddToScheme (see api/v1alpha1/groupversion_info.go).
var scheme = runtime.NewScheme()

func init() {
	utilRuntimeMustRegister(clientgoscheme.AddToScheme)
	utilRuntimeMustRegister(opsv1alpha1.AddToScheme)
}

// utilRuntimeMustRegister panics on registration failure - equivalent to
// the standard kubebuilder-generated `utilruntime.Must(...)`, spelled out
// here to avoid pulling in that whole package for one helper.
func utilRuntimeMustRegister(addToScheme func(*runtime.Scheme) error) {
	if err := addToScheme(scheme); err != nil {
		panic(err)
	}
}

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	logger := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		logger.Error(err, "unable to start manager")
		os.Exit(1)
	}

	reconciler := &controller.Reconciler{Client: mgr.GetClient()}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to set up CredentialRotation controller")
		os.Exit(1)
	}

	logger.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
