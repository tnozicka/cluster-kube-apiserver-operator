package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"text/template"
	"time"

	"github.com/ghodss/yaml"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clientcmdapiv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/crypto"

	"github.com/openshift/cluster-kube-apiserver-operator/pkg/operator/v311_00_assets"
)

const (
	KubeApiserverStaticPodFileName = "kube-apiserver-pod.yaml"
	RecoveryPodFileName            = "recovery-kube-apiserver-pod.yaml"
	RecoveryPodNamespace           = "kube-system"
	RecoveryCofigFileName          = "config.yaml"
	AdminKubeconfigFileName        = "admin.kubeconfig"

	RecoveryPodAsset    = "v3.11.0/kube-apiserver/recovery-pod.yaml"
	RecoveryConfigAsset = "v3.11.0/kube-apiserver/recovery-config.yaml"
)

var (
	Scheme = runtime.NewScheme()
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})
	utilruntime.Must(corev1.AddToScheme(Scheme))
}

type Apiserver struct {
	KubeApiserverImage string
	PodManifestDir     string
	ResourceDirPath    string
	RecoveryDirPath    string

	kubeApiserverStaticPod *corev1.Pod
	restConfig             *rest.Config
	kubeClientSet          *kubernetes.Clientset
}

func (s *Apiserver) GetCurrentApiserverManifest() (*corev1.Pod, error) {
	if s.kubeApiserverStaticPod != nil {
		return s.kubeApiserverStaticPod, nil
	}

	kubeApiserverManifestPath := path.Join(s.PodManifestDir, KubeApiserverStaticPodFileName)
	f, err := os.OpenFile(kubeApiserverManifestPath, os.O_RDONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open file %q: %v", kubeApiserverManifestPath, err)
	}
	defer f.Close()

	buf := bytes.NewBuffer(nil)
	_, err = io.Copy(buf, f)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %q: %v", kubeApiserverManifestPath, err)
	}

	obj, err := runtime.Decode(Codecs.UniversalDecoder(corev1.SchemeGroupVersion), buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to decode file %q: %v", kubeApiserverManifestPath, err)
	}

	// TODO: support conversions if the object is in different but convertible version
	kubeApiserverStaticPod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("unsupported type: kubeApiserverStaticPodManifest is not type *corev1.Pod but %T", obj)
	}

	s.kubeApiserverStaticPod = kubeApiserverStaticPod

	return s.kubeApiserverStaticPod, nil
}

func (s *Apiserver) GetResourceDirPath() (string, error) {
	if s.ResourceDirPath != "" {
		return s.ResourceDirPath, nil
	}

	pod, err := s.GetCurrentApiserverManifest()
	if err != nil {
		return "", fmt.Errorf("failed to read apiserver pod manifest: %v", err)
	}

	resourceDir, err := getVolumeHostPathPath("resource-dir", pod.Spec.Volumes)
	if err != nil {
		return "", fmt.Errorf("failed to find resource-dir: %v", err)
	}

	s.ResourceDirPath = *resourceDir

	return s.ResourceDirPath, nil
}

func (s *Apiserver) GetRecoveryDirPath() (string, error) {
	if s.RecoveryDirPath != "" {
		return s.RecoveryDirPath, nil
	}

	// Create tmp dir for recovery apiserver
	recoveryDir, err := ioutil.TempDir("", "kube-recovery-apiserver")
	if err != nil {
		return "", fmt.Errorf("failed to create tmpDir: %v", err)
	}

	s.RecoveryDirPath = recoveryDir

	return s.RecoveryDirPath, nil
}

func (s *Apiserver) RestConfig() (*rest.Config, error) {
	if s.restConfig == nil {
		return nil, errors.New("no rest config is set yet")
	}

	return s.restConfig, nil
}

func (s *Apiserver) KubeConfig() (*clientcmdapiv1.Config, error) {
	restConfig, err := s.RestConfig()
	if err != nil {
		return nil, err
	}

	return &clientcmdapiv1.Config{
		APIVersion: "v1",
		Clusters: []clientcmdapiv1.NamedCluster{
			{
				Name: "recovery",
				Cluster: clientcmdapiv1.Cluster{
					CertificateAuthority: restConfig.CAFile,
					Server:               restConfig.Host,
				},
			},
		},
		Contexts: []clientcmdapiv1.NamedContext{
			{
				Name: "admin",
				Context: clientcmdapiv1.Context{
					Cluster:  "recovery",
					AuthInfo: "admin",
				},
			},
		},
		CurrentContext: "admin",
		AuthInfos: []clientcmdapiv1.NamedAuthInfo{
			{
				Name: "admin",
				AuthInfo: clientcmdapiv1.AuthInfo{
					ClientCertificateData: restConfig.CertData,
					ClientKeyData:         restConfig.KeyData,
				},
			},
		},
	}, nil
}

func (s *Apiserver) GetKubeClientset() (*kubernetes.Clientset, error) {
	if s.kubeClientSet != nil {
		return s.kubeClientSet, nil
	}

	restConfig, err := s.RestConfig()
	if err != nil {
		return nil, err
	}

	kubeClientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubernetes clientset: %v", err)
	}

	s.kubeClientSet = kubeClientset

	return s.kubeClientSet, nil
}

func (s *Apiserver) recoveryPod() (*corev1.Pod, error) {
	recoveryDirPath, err := s.GetRecoveryDirPath()
	if err != nil {
		return nil, fmt.Errorf("failed to get recovery dir: %v", err)
	}

	// Create the manifest to run recovery apiserver
	recoveryPodTemplateBytes, err := v311_00_assets.Asset(RecoveryPodAsset)
	if err != nil {
		return nil, fmt.Errorf("fail to find internal recovery pod asset %q: %v", RecoveryPodAsset, err)
	}

	// Process the template
	t, err := template.New("recovery-pod-template").Parse(string(recoveryPodTemplateBytes))
	if err != nil {
		return nil, fmt.Errorf("fail to parse internal recovery pod template %q: %v", RecoveryPodAsset, err)
	}

	recoveryPodBuffer := bytes.NewBuffer(nil)
	err = t.Execute(recoveryPodBuffer, struct {
		KubeApiserverImage string
		ResourceDir        string
	}{
		KubeApiserverImage: s.KubeApiserverImage,
		ResourceDir:        recoveryDirPath,
	})
	if err != nil {
		return nil, fmt.Errorf("fail to execute internal recovery pod template %q: %v", RecoveryPodAsset, err)
	}

	recoveryPodObj, err := runtime.Decode(Codecs.UniversalDecoder(corev1.SchemeGroupVersion), recoveryPodBuffer.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to decode internal recovery pod %q: %v", RecoveryPodAsset, err)
	}

	recoveryPod, ok := recoveryPodObj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("unsupported type: internal recovery pod is not type *corev1.Pod but %T", recoveryPod)
	}

	return recoveryPod, nil
}

func (s *Apiserver) Create() error {
	resourceDirPath, err := s.GetResourceDirPath()
	if err != nil {
		return fmt.Errorf("failed to get resource dir: %v", err)
	}

	recoveryDirPath, err := s.GetRecoveryDirPath()
	if err != nil {
		return fmt.Errorf("failed to get recovery dir: %v", err)
	}

	// Copy certs for accessing etcd
	for src, dest := range map[string]string{
		"secrets/etcd-client/tls.key":              "etcd-client.key",
		"secrets/etcd-client/tls.crt":              "etcd-client.crt",
		"configmaps/etcd-serving-ca/ca-bundle.crt": "etcd-serving-ca-bundle.crt",
	} {
		err = copyFile(path.Join(resourceDirPath, src), path.Join(recoveryDirPath, dest))
		if err != nil {
			return err
		}
	}

	// We are creating only temporary certificates to start the recovery apiserver.
	// A week seem reasonably high for a debug session, while it is easy to create a new one.
	certValidity := 7 * 24 * time.Hour

	// Create root CA
	rootCaConfig, err := crypto.MakeSelfSignedCAConfigForDuration("localhost", certValidity)
	if err != nil {
		return fmt.Errorf("failed to create root-signer CA: %v", err)
	}

	servingCaCertPath := path.Join(recoveryDirPath, "serving-ca.crt")
	err = rootCaConfig.WriteCertConfigFile(servingCaCertPath, path.Join(recoveryDirPath, "serving-ca.key"))
	if err != nil {
		return fmt.Errorf("failed to write root-signer files: %v", err)
	}

	// Create config for recovery apiserver
	recoveryConfigBytes, err := v311_00_assets.Asset(RecoveryConfigAsset)
	if err != nil {
		return fmt.Errorf("fail to find internal recovery config asset %q: %v", RecoveryConfigAsset, err)
	}

	recoveryConfigPath := path.Join(recoveryDirPath, RecoveryCofigFileName)
	err = ioutil.WriteFile(recoveryConfigPath, recoveryConfigBytes, 644)
	if err != nil {
		return fmt.Errorf("failed to write recovery config %q: %v", recoveryConfigPath, err)
	}

	recoveryPod, err := s.recoveryPod()
	if err != nil {
		return fmt.Errorf("failed to create recovery pod: %v", err)
	}

	recoveryPodBytes, err := yaml.Marshal(recoveryPod)
	if err != nil {
		return fmt.Errorf("failed to marshal recovery pod: %v", err)
	}

	recoveryPodManifestPath := path.Join(s.PodManifestDir, RecoveryPodFileName)
	err = ioutil.WriteFile(recoveryPodManifestPath, recoveryPodBytes, 644)
	if err != nil {
		return fmt.Errorf("failed to write recovery pod manifest %q: %v", recoveryPodManifestPath, err)
	}

	// Create client cert
	ca := crypto.CA{
		Config:          rootCaConfig,
		SerialGenerator: &crypto.RandomSerialGenerator{},
	}

	// Create client certificates for system:admin
	clientCert, err := ca.MakeClientCertificateForDuration(&user.DefaultInfo{Name: "system:admin"}, certValidity)
	if err != nil {
		return fmt.Errorf("failed to create client certificate: %v", err)
	}

	clientCertBytes, clientKeyBytes, err := clientCert.GetPEMBytes()

	s.restConfig = &rest.Config{
		Host: "https://localhost:7443",
		TLSClientConfig: rest.TLSClientConfig{
			CAFile:   servingCaCertPath,
			CertData: clientCertBytes,
			KeyData:  clientKeyBytes,
		},
	}

	kubeconfig, err := s.KubeConfig()
	if err != nil {
		return fmt.Errorf("failed to create kubeconfig: %v", err)
	}

	kubeconfigBytes, err := yaml.Marshal(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to marshal kubeconfig: %v", err)
	}

	kubeconfigPath := path.Join(recoveryDirPath, AdminKubeconfigFileName)
	err = ioutil.WriteFile(kubeconfigPath, kubeconfigBytes, 600)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig %q: %v", kubeconfigPath, err)
	}

	return nil
}

func (s *Apiserver) WaitForHealthz(ctx context.Context) error {
	// We don't have to worry about connecting to an old, terminating apiserver
	// as our client certs are unique to this instance

	kubeClientset, err := s.GetKubeClientset()
	if err != nil {
		return err
	}

	return wait.PollUntil(500*time.Millisecond, func() (bool, error) {
		req := kubeClientset.RESTClient().Get().AbsPath("/healthz")

		klog.V(1).Infof("Waiting for recovery apiserver to come up at %q", req.URL())

		_, err = req.DoRaw()
		if err != nil {
			klog.V(5).Infof("apiserver returned error: %v", err)
			return false, nil
		}

		return true, nil
	}, ctx.Done())
}

func (s *Apiserver) Destroy() error {
	recoveryPodManifestPath := path.Join(s.PodManifestDir, RecoveryPodFileName)
	err := os.Remove(recoveryPodManifestPath)
	if err != nil {
		return fmt.Errorf("failed to remove recovery pod manifest %q: %v", recoveryPodManifestPath, err)
	}

	// TODO: consider removing the recoveryDir
	// (The dir is tmp by default anyways and putting a wrong path here could be fatal as it would have to recursively delete.)

	return nil
}

func copyFile(src, dest string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open file %q: %v", src, err)
	}
	defer srcFile.Close()

	srcFileInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file %q: %v", src, err)
	}

	if srcFileInfo.IsDir() {
		return fmt.Errorf("can't copy file %q because it is a directory", src)
	}

	destFile, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, srcFileInfo.Mode().Perm())
	if err != nil {
		return fmt.Errorf("failed to open file %q: %v", dest, err)
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, srcFile)
	if err != nil {
		return fmt.Errorf("failed to copy file %q into %q: %v", src, dest, err)
	}

	return nil
}

func getVolumeHostPathPath(volumeName string, volumes []corev1.Volume) (*string, error) {
	for _, volume := range volumes {
		if volume.Name == volumeName {
			if volume.HostPath == nil {
				return nil, errors.New("volume doesn't have hostPath set")
			}
			if volume.HostPath.Path == "" {
				return nil, errors.New("volume hostPath shall not be empty")
			}

			return &volume.HostPath.Path, nil
		}
	}

	return nil, fmt.Errorf("volume %q not found", volumeName)
}
