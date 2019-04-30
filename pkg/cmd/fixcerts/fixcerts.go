package fixcerts

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/openshift/cluster-kube-apiserver-operator/pkg/operator"
	"github.com/openshift/library-go/pkg/operator/staticpod/installerpod"
	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/watch"
	"k8s.io/klog"

	configeversionedclient "github.com/openshift/client-go/config/clientset/versioned"
	configexternalinformers "github.com/openshift/client-go/config/informers/externalversions"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorexternalinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-kube-apiserver-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-kube-apiserver-operator/pkg/operator/certrotationcontroller"
	"github.com/openshift/cluster-kube-apiserver-operator/pkg/recovery"
)

var (
	Scheme = runtime.NewScheme()
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})
	utilruntime.Must(corev1.AddToScheme(Scheme))
}

type Options struct {
	// TODO: merge with CreateOptions
	KubeApiserverImage string
	PodManifestDir     string
	Timeout            time.Duration
}

func NewCommand() *cobra.Command {
	o := &Options{
		KubeApiserverImage: "", // TODO: set the public pullspec
		PodManifestDir:     "/etc/kubernetes/manifests",
		Timeout:            5 * time.Minute,
	}

	cmd := &cobra.Command{
		Use: "fix-certs",
		RunE: func(cmd *cobra.Command, args []string) error {
			err := o.Complete()
			if err != nil {
				return err
			}

			err = o.Validate()
			if err != nil {
				return err
			}

			err = o.Run()
			if err != nil {
				return err
			}

			return nil
		},

		SilenceErrors: true,
		SilenceUsage:  true,
	}

	// TODO: add/prettify descriptions
	cmd.Flags().StringVar(&o.KubeApiserverImage, "kube-apiserver-image", o.KubeApiserverImage, "")
	cmd.Flags().StringVar(&o.PodManifestDir, "pod-manifest-dir", o.PodManifestDir, "directory for the static pod manifes")
	cmd.Flags().DurationVar(&o.Timeout, "timeout", o.Timeout, "timeout, 0 means infinite")

	return cmd
}

func (o *Options) Complete() error {
	return nil
}

func (o *Options) Validate() error {
	if o.KubeApiserverImage == "" {
		return errors.New("missing required flag --kube-apiserver-image")
	}

	if o.PodManifestDir == "" {
		return errors.New("pod-manifest-dir shall not be empty")
	}

	return nil
}

func (o *Options) Run() error {
	ctx, cancel := watch.ContextWithOptionalTimeout(context.Background(), o.Timeout)
	defer cancel()
	// TODO: hook up signals

	apiserver := &recovery.Apiserver{
		KubeApiserverImage: o.KubeApiserverImage,
		PodManifestDir:     o.PodManifestDir,
	}
	defer apiserver.Destroy()

	err := apiserver.Create()
	if err != nil {
		return fmt.Errorf("failed to create recovery apiserver: %v", err)
	}

	klog.Info("Waiting for recovery apiserver to come up")
	err = apiserver.WaitForHealthz(ctx)
	if err != nil {
		return fmt.Errorf("failed to wait for recovery apiserver to be ready: %v", err)
	}
	klog.Info("Recovery apiserver is up")

	// Run cert recovery

	// We already have kubeClient created
	kubeClient, err := apiserver.GetKubeClientset()
	if err != nil {
		return fmt.Errorf("failed to get kubernetes clientset: %v", err)
	}

	restConfig, err := apiserver.RestConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubernetes rest config: %v", err)
	}

	operatorConfigClient, err := operatorversionedclient.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	configClient, err := configeversionedclient.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create config client: %v", err)
	}

	operatorConfigInformers := operatorexternalinformers.NewSharedInformerFactory(operatorConfigClient, 10*time.Minute)

	configInformers := configexternalinformers.NewSharedInformerFactory(configClient, 10*time.Minute)

	operatorClient := &operatorclient.OperatorClient{
		Informers: operatorConfigInformers,
		Client:    operatorConfigClient.OperatorV1(),
	}

	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
		kubeClient,
		"",
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		operatorclient.TargetNamespace,
		operatorclient.OperatorNamespace,
		"kube-system",
	)

	eventRecorder := events.NewKubeRecorder(kubeClient.CoreV1().Events(""), "fix-certs (CLI)", &corev1.ObjectReference{
		APIVersion: "v1",
		Kind:       "namespace",
		Name:       "kube-system",
	}) // fake

	certRotationController, err := certrotationcontroller.NewCertRotationController(
		kubeClient,
		operatorClient,
		configInformers,
		kubeInformersForNamespaces,
		eventRecorder.WithComponentSuffix("cert-rotation-controller"),
		0,
	)
	if err != nil {
		return err
	}

	certRotationCtx, certRotationCtxCancel := context.WithCancel(ctx)
	defer certRotationCtxCancel()

	certRotationWg := sync.WaitGroup{}
	go func() {
		certRotationWg.Add(1)
		defer certRotationWg.Done()

		certRotationController.Run(1, certRotationCtx.Done())
	}()

	klog.Info("Waiting for certs to be refreshed...")
	// FIXME: wait for valid certs
	// time.Sleep(5*time.Minute)
	time.Sleep(5 * time.Second)
	klog.Info("Certificates refreshed.")

	klog.V(1).Info("Stopping CertRotationController...")
	certRotationCtxCancel()
	certRotationWg.Wait()
	klog.V(1).Info("Stopped CertRotationController...")

	apiserverRecoveryDirPath, err := apiserver.GetRecoveryDirPath()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %v", err)
	}

	kubeconfigPath := path.Join(apiserverRecoveryDirPath, recovery.AdminKubeconfigFileName)

	var apiserverRevisionConfigMaps []string
	var apiserverOptionalRevisionConfigMaps []string
	for _, cm := range operator.RevisionConfigMaps {
		if cm.Optional {
			apiserverOptionalRevisionConfigMaps = append(apiserverOptionalRevisionConfigMaps, cm.Name)
		} else {
			apiserverRevisionConfigMaps = append(apiserverRevisionConfigMaps, cm.Name)
		}
	}

	var apiserverRevisionSecrets []string
	var apiserverOptionalRevisionSecrets []string
	for _, cm := range operator.RevisionSecrets {
		if cm.Optional {
			apiserverOptionalRevisionSecrets = append(apiserverOptionalRevisionSecrets, cm.Name)
		} else {
			apiserverRevisionSecrets = append(apiserverRevisionSecrets, cm.Name)
		}
	}

	var apiserverCertConfigMaps []string
	var apiserverOptionalCertConfigMaps []string
	for _, cm := range operator.CertConfigMaps {
		if cm.Optional {
			apiserverOptionalCertConfigMaps = append(apiserverOptionalCertConfigMaps, cm.Name)
		} else {
			apiserverCertConfigMaps = append(apiserverCertConfigMaps, cm.Name)
		}
	}

	var apiserverCertSecrets []string
	var apiserverOptionalCertSecrets []string
	for _, cm := range operator.CertSecrets {
		if cm.Optional {
			apiserverOptionalCertSecrets = append(apiserverOptionalCertSecrets, cm.Name)
		} else {
			apiserverCertSecrets = append(apiserverCertSecrets, cm.Name)
		}
	}

	// Install new kube-apiserver

	// TODO: we likely need to start revision controller to take the snapshots

	apiserverOperatorConfig, err := operatorConfigClient.OperatorV1().KubeAPIServers().Get("cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get apiserver operator config")
	}

	apiserverLatestRevision := strconv.Itoa(int(apiserverOperatorConfig.Status.LatestAvailableRevision))
	klog.V(1).Info("Installing apiserver latest revision %q", apiserverLatestRevision)

	kubeApiserverOptions := installerpod.InstallOptions{
		KubeConfig:             kubeconfigPath,
		Revision:               apiserverLatestRevision,
		Namespace:              "openshift-kube-apiserver",
		PodManifestDir:         o.PodManifestDir,
		ResourceDir:            "/etc/kubernetes/static-pod-resources/kube-apiserver-recovery",
		CertDir:                "/etc/kubernetes/static-pod-resources/kube-apiserver-certs",
		PodConfigMapNamePrefix: "kube-apiserver-pod",
		Timeout:                120 * time.Second,

		ConfigMapNamePrefixes:         apiserverRevisionConfigMaps,
		OptionalConfigMapNamePrefixes: apiserverOptionalRevisionConfigMaps,

		SecretNamePrefixes:         apiserverRevisionSecrets,
		OptionalSecretNamePrefixes: apiserverOptionalRevisionSecrets,

		CertConfigMapNamePrefixes:         apiserverCertConfigMaps,
		OptionalCertConfigMapNamePrefixes: apiserverOptionalCertConfigMaps,

		CertSecretNames:                apiserverCertSecrets,
		OptionalCertSecretNamePrefixes: apiserverOptionalCertSecrets,
	}

	err = kubeApiserverOptions.Complete()
	if err != nil {
		return fmt.Errorf("failed to complete kube apiserver installer options: %v", err)
	}

	err = kubeApiserverOptions.Validate()
	if err != nil {
		return fmt.Errorf("failed to validate kube apiserver installer options: %v", err)
	}

	err = kubeApiserverOptions.Run(ctx)
	if err != nil {
		return fmt.Errorf("failed to run kube apiserver installer: %v", err)
	}

	// TODO: Install new controller manager
	// resource sync

	// TODO: Install new scheduler
	// resource sync

	// TODO: Install new cert for kubelet
	// Might need to be manual

	return nil
}
