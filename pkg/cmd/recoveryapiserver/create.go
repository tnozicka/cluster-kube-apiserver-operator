package recoveryapiserver

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/spf13/cobra"

	"k8s.io/client-go/tools/watch"

	"github.com/openshift/cluster-kube-apiserver-operator/pkg/recovery"
)

type CreateOptions struct {
	Options

	KubeApiserverImage string
	Timeout            time.Duration
	Wait               bool
}

func NewCreateCommand() *cobra.Command {
	o := &CreateOptions{
		Options:            NewDefaultOptions(),
		KubeApiserverImage: "", // TODO: set the public pullspec
		Timeout:            5 * time.Minute,
		Wait:               true,
	}

	cmd := &cobra.Command{
		Use: "create",
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
	}

	// TODO: add/prettify descriptions
	AddFlags(cmd, &o.Options)

	cmd.Flags().StringVar(&o.KubeApiserverImage, "kube-apiserver-image", o.KubeApiserverImage, "")
	cmd.Flags().DurationVar(&o.Timeout, "timeout", o.Timeout, "timeout, 0 means infinite")
	cmd.Flags().BoolVar(&o.Wait, "wait", o.Wait, "wait for recovery apiserver to become ready")

	return cmd
}

func (o *CreateOptions) Complete() error {
	err := o.Options.Complete()
	if err != nil {
		return err
	}

	return nil
}

func (o *CreateOptions) Validate() error {
	err := o.Options.Validate()
	if err != nil {
		return err
	}

	if o.KubeApiserverImage == "" {
		return errors.New("missing required flag --kube-apiserver-image")
	}

	return nil
}

func (o *CreateOptions) Run() error {
	ctx, cancel := watch.ContextWithOptionalTimeout(context.Background(), o.Timeout)
	defer cancel()
	// TODO: hook up signals

	apiserver := &recovery.Apiserver{
		KubeApiserverImage: o.KubeApiserverImage,
		PodManifestDir:     o.PodManifestDir,
	}

	err := apiserver.Create()
	if err != nil {
		return fmt.Errorf("failed to create recovery apiserver: %v", err)
	}

	recoveryDirPath, err := apiserver.GetRecoveryDirPath()
	if err != nil {
		return fmt.Errorf("failed to get recovery dir: %v", err)
	}

	kubeconfigPath := path.Join(recoveryDirPath, recovery.AdminKubeconfigFileName)
	fmt.Printf("system:admin kubeconfig written to %q\n", kubeconfigPath)

	if !o.Wait {
		return nil
	}

	fmt.Printf("Waiting for recovery apiserver to come up.")
	err = apiserver.WaitForHealthz(ctx)
	if err != nil {
		return fmt.Errorf("failed to wait for recovery apiserver to be ready: %v", err)
	}
	fmt.Println("Recovery apiserver is up.")

	return nil
}
