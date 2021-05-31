package release

import (
	"context"
	"errors"
	"fmt"
	v1 "github.com/fluxcd/helm-operator/pkg/apis/helm.fluxcd.io/v1"
	"github.com/fluxcd/helm-operator/pkg/helm"
	"k8s.io/klog"
	"os/exec"
	"time"
)

const (
	AppIdLabelKey       = "o.app.id"
	ComponentIdLabelKey = "oam.runtime.component.id"
	IstioEnableLabelKey = "istio-injection"
)

func labelResources(hr *v1.HelmRelease, rel *helm.Release) error {
	if hr.Spec.AppId == "" || hr.Spec.ComponentId == "" {
		return nil
	}
	objs := releaseManifestToUnstructured(rel.Manifest)
	errs := errCollection{}
	for namespace, res := range namespacedResourceMap(objs, rel.Namespace) {
		args := []string{"label", "--overwrite"}
		args = append(args, "--namespace", namespace)
		args = append(args, res...)
		args = append(args, fmt.Sprintf("%s=%s", AppIdLabelKey, hr.Spec.AppId), fmt.Sprintf("%s=%s", ComponentIdLabelKey, hr.Spec.ComponentId))

		if hr.Spec.IstioEnabled {
			args = append(args,fmt.Sprintf("%s=%s",IstioEnableLabelKey,"enabled"))
		}

		// The timeout is set to a high value as it may take some time
		// to label large umbrella charts.
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		klog.Infof("start to exec label resources , cmd : %v",args)
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		output, err := cmd.CombinedOutput()
		if err != nil && len(output) > 0 {
			err = errors.New(string(output))
			errs = append(errs, err)
		}
	}

	if !errs.Empty() {
		return errs
	}
	return nil

}
