package recoveryapiserver

import (
	"errors"

	"github.com/spf13/cobra"
)

type Options struct {
	PodManifestDir string
}

func NewDefaultOptions() Options {
	return Options{
		PodManifestDir: "/etc/kubernetes/manifests",
	}
}

func (o *Options) Complete() error {
	return nil
}

func (o *Options) Validate() error {
	if o.PodManifestDir == "" {
		return errors.New("pod-manifest-dir shall not be empty")
	}

	return nil
}

func AddFlags(cmd *cobra.Command, o *Options) {
	// TODO: add/prettify descriptions

	cmd.Flags().StringVar(&o.PodManifestDir, "pod-manifest-dir", o.PodManifestDir, "directory for the static pod manifest")
}
