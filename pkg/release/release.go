package release

import (
	"bytes"
	"context"
	"fmt"
	"helm.sh/helm/v3/pkg/postrender"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"strconv"
	"time"

	"github.com/go-kit/kit/log"

	apiV1 "github.com/lstack-org/helm-operator/pkg/apis/helm.fluxcd.io/v1"
	"github.com/lstack-org/helm-operator/pkg/chartsync"
	v1client "github.com/lstack-org/helm-operator/pkg/client/clientset/versioned/typed/helm.fluxcd.io/v1"
	"github.com/lstack-org/helm-operator/pkg/helm"
	helmV3 "github.com/lstack-org/helm-operator/pkg/helm/v3"
	"github.com/lstack-org/helm-operator/pkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Config holds the configuration for releases.
type Config struct {
	ChartCache         string
	UpdateDeps         bool
	LogDiffs           bool
	DefaultHelmVersion string
}

// WithDefaults sets the default values for the release config.
func (c Config) WithDefaults() Config {
	if c.ChartCache == "" {
		c.ChartCache = "/tmp"
	}
	return c
}

// Release holds the elements required to perform a Helm release,
// and provides the methods to perform a sync or uninstall.
type Release struct {
	logger       log.Logger
	helmClients  *helm.Clients
	coreV1Client corev1client.CoreV1Interface
	hrClient     v1client.HelmV1Interface
	gitChartSync *chartsync.GitChartSync
	config       Config
	converter    helmV3.Converter
}

// New returns a new instance of Release
func New(logger log.Logger, helmClients *helm.Clients, coreV1Client corev1client.CoreV1Interface, hrClient v1client.HelmV1Interface,
	gitChartSync *chartsync.GitChartSync, config Config, converter helmV3.Converter) *Release {
	r := &Release{
		logger:       logger,
		helmClients:  helmClients,
		coreV1Client: coreV1Client,
		hrClient:     hrClient,
		gitChartSync: gitChartSync,
		config:       config.WithDefaults(),
		converter:    converter,
	}
	return r
}

// Sync synchronizes the given HelmRelease with Helm.
func (r *Release) Sync(hr *apiV1.HelmRelease) (err error) {
	client, ok := r.helmClients.Load(hr.GetHelmVersion(r.config.DefaultHelmVersion))
	if !ok {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.GetTargetNamespace()), hr, apiV1.HelmReleasePhaseFailed)
		return fmt.Errorf("no client found for Helm '%s'", r.config.DefaultHelmVersion)
	}
	logger := releaseLogger(r.logger, client, hr)

	defer func(start time.Time) {
		ObserveRelease(start, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	defer status.SetObservedGeneration(r.hrClient.HelmReleases(hr.Namespace), hr, hr.Generation)

	logger.Log("info", "starting sync run")

	chart, cleanup, err := r.prepareChart(client, hr)
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseChartFetchFailed)
		err = fmt.Errorf("failed to prepare chart for release: %w", err)
		logger.Log("error", err)
		return
	}
	if cleanup != nil {
		defer cleanup()
	}
	if chart.changed {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseChartFetched)
	}

	var values []byte
	values, err = composeValues(r.coreV1Client, hr, chart.chartPath)
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.GetTargetNamespace()), hr, apiV1.HelmReleasePhaseFailed)
		err = fmt.Errorf("failed to compose values for release: %w", err)
		logger.Log("error", err)
		return
	}
	var action action
	var curRel *helm.Release
	action, curRel, err = r.determineSyncAction(client, hr, chart)
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.GetTargetNamespace()), hr, apiV1.HelmReleasePhaseFailed)
		err = fmt.Errorf("failed to determine sync action for release: %w", err)
		logger.Log("error", err)
		return
	}
	return r.run(logger, client, action, hr, curRel, chart, values)
}

// Uninstalls removes the Helm release for the given HelmRelease,
// and the git chart source if present.
func (r *Release) Uninstall(hr *apiV1.HelmRelease) error {
	client, ok := r.helmClients.Load(hr.GetHelmVersion(r.config.DefaultHelmVersion))
	if !ok {
		return fmt.Errorf(`no client found for Helm '%s'`, r.config.DefaultHelmVersion)
	}
	logger := releaseLogger(r.logger, client, hr)
	return r.run(logger, client, UninstallAction, hr, nil, chart{}, nil)
}

// chart is a reference to a Helm chart used internally during the release.
type chart struct {
	chartPath string
	revision  string
	changed   bool
}

// prepareChart returns the chart for the configured chart source in
// the given HelmRelease, or an error.
func (r *Release) prepareChart(client helm.Client, hr *apiV1.HelmRelease) (chart, func() error, error) {
	var chartPath, revision string
	var changed bool
	switch {
	case hr.Spec.GitChartSource != nil && hr.Spec.GitURL != "" && hr.Spec.Path != "":
		var export *git.Export
		var err error

		export, revision, err = r.gitChartSync.GetMirrorCopy(hr)
		if err != nil {
			return chart{}, nil, err
		}
		chartPath = filepath.Join(export.Dir(), hr.Spec.GitChartSource.Path)
		changed = func() bool {
			i, _ := export.ChangedFiles(context.Background(), hr.Status.LastAttemptedRevision, []string{hr.Spec.GitChartSource.Path})
			return 0 < len(i)
		}()
		if r.config.UpdateDeps && !hr.Spec.GitChartSource.SkipDepUpdate {
			if err := client.DependencyUpdate(chartPath); err != nil {
				return chart{}, nil, err
			}
		}
		return chart{chartPath, revision, changed}, export.Clean, nil
	case hr.Spec.RepoChartSource != nil && hr.Spec.RepoURL != "" && hr.Spec.Name != "" && hr.Spec.Version != "":
		var err error

		chartPath, _, err = chartsync.EnsureChartFetched(client, r.config.ChartCache, hr.Spec.RepoChartSource)
		if err != nil {
			return chart{}, nil, err
		}
		revision, err = client.GetChartRevision(chartPath)
		if err != nil {
			return chart{}, nil, err
		}
		changed = hr.Status.LastAttemptedRevision != revision
	case hr.Spec.Customize != nil && hr.Spec.Customize.Key != "":
		var err error

		chartPath, err = chartsync.DownloadFile(hr.Spec.Customize.Key, r.config.ChartCache, hr.Spec.Customize.UseCache)
		if err != nil {
			return chart{}, nil, err
		}

		revision, err = client.GetChartRevision(chartPath)
		if err != nil {
			return chart{}, nil, err
		}
		changed = hr.Status.LastAttemptedRevision != revision
	case hr.Spec.Oss != nil:
		var err error

		provider, err := chartsync.NewProvider(hr.Spec.Oss, r.config.ChartCache)
		if err != nil {
			return chart{}, nil, err
		}

		chartPath, err = provider.DownloadFile(hr.Spec.Oss.UseCache)
		if err != nil {
			return chart{}, nil, err
		}

		revision, err = client.GetChartRevision(chartPath)
		if err != nil {
			return chart{}, nil, err
		}
		changed = hr.Status.LastAttemptedRevision != revision
	default:
		return chart{}, nil, fmt.Errorf("could not find valid chart source configuration for release")
	}
	return chart{chartPath, revision, changed}, nil, nil
}

type action string

const (
	InstallAction       action = "install"
	UpgradeAction       action = "upgrade"
	MigrateAction       action = "migrate"
	SkipAction          action = "skip"
	RollbackAction      action = "rollback"
	UninstallAction     action = "uninstall"
	DryRunCompareAction action = "dry-run-compare"
	AnnotateAction      action = "annotate"
	TestAction          action = "test"
)

const (
	MigrateAnnotation string = "helm.fluxcd.io/migrate"
)

// shouldSync determines if the given HelmRelease should be synced
// with Helm. The cheapest checks which do not require a dry-run are
// consulted first (e.g. is this our first sync, have we already seen
// this revision of the resource); before running the dry-run release to
// determine if any undefined mutations have occurred. It returns a
// booleans indicating if the release should be synced, or an error.
func (r *Release) determineSyncAction(client helm.Client, hr *apiV1.HelmRelease, chart chart) (action, *helm.Release, error) {
	curRel, err := client.Get(hr.GetReleaseName(), helm.GetOptions{Namespace: hr.GetTargetNamespace()})
	if err != nil {
		return SkipAction, nil, fmt.Errorf("failed to retrieve Helm release: %w", err)
	}

	// If there is no existing release, we should install.
	if curRel == nil {
		// Handling of incoming V3 HelmRelease objects
		// ===========================================
		//
		// Behavior without migration:
		// 	- If neither v2 nor v3 release exists, proceed to InstallAction
		// 	- If a v3 release exists, proceed into UpgradeAction
		// 	- If a v2 release exists, the InstallAction would fail with an "unable to resolve conflicts" error
		//
		// Behavior with migration:
		// 	- If neither v2 nor v3 release exists, proceed to InstallAction
		// 	- If a v3 release exists, skip migration logic and proceed into UpgradeAction
		// 	- If a v2 release exists and the migrate annotation exists, run a MigrateAction -> UpgradeAction
		if _, ok := hr.GetAnnotations()[MigrateAnnotation]; ok {
			switch hr.GetHelmVersion(r.config.DefaultHelmVersion) {
			case string(apiV1.HelmV3):
				v2ReleaseExists, err := r.converter.V2ReleaseExists(hr.GetReleaseName())
				if err != nil {
					return SkipAction, nil, fmt.Errorf("failed to retrieve Helm v2 release while attempting migration: %w", err)
				}
				if v2ReleaseExists {
					return MigrateAction, nil, nil
				}
			}
		}
		return InstallAction, nil, nil
	}

	// Check if the release is managed by our resource: if the release is
	// appears to be managed by another `HelmRelease` resource, or an error
	// is returned, we skip to avoid conflicts.
	managedBy, antecedent, err := managedByHelmRelease(curRel, *hr)
	if err != nil {
		return SkipAction, nil, fmt.Errorf("failed to determine ownership over release: %w", err)
	}
	if !managedBy {
		return SkipAction, nil, fmt.Errorf("release appears to be managed by '%s'", antecedent)
	}

	// If the current state of the release does not allow us to safely
	// upgrade, we skip.
	if s := curRel.Info.Status; !s.AllowsUpgrade() {
		return SkipAction, nil, fmt.Errorf("status '%s' of release does not allow a safe upgrade", s.String())
	}

	// If this revision of the `HelmRelease` has not been synchronized
	// yet, we attempt an upgrade.
	if !status.HasSynced(hr) {
		return UpgradeAction, curRel, nil
	}

	// The release has been rolled back, inspect state.
	if status.HasRolledBack(hr) {
		if chart.changed || status.ShouldRetryUpgrade(hr) {
			return UpgradeAction, curRel, nil
		}
		hist, err := client.History(hr.GetReleaseName(), helm.HistoryOptions{Namespace: hr.GetTargetNamespace(), Max: hr.GetMaxHistory()})
		if err != nil {
			return SkipAction, nil, fmt.Errorf("failed to retreive history for rolled back release: %w", err)
		}
		for _, r := range hist {
			if r.Info.Status == helm.StatusFailed || r.Info.Status == helm.StatusSuperseded {
				curRel = r
				break
			}
		}
	} else if chart.changed {
		return UpgradeAction, curRel, nil
	}
	return DryRunCompareAction, curRel, nil
}

// run starts on the given action and loops through the release cycle.
func (r *Release) run(logger log.Logger, client helm.Client, action action, hr *apiV1.HelmRelease, curRel *helm.Release,
	chart chart, values []byte) error {
	var newRel *helm.Release
	errs := errCollection{}
next:
	var err error
	switch action {
	case DryRunCompareAction:
		logger.Log("info", fmt.Sprintf("running dry-run upgrade to compare with release version '%d'", curRel.Version), "action", action)
		var diff string
		newRel, diff, err = r.dryRunCompare(client, curRel, hr, chart, values)
		if err != nil {
			status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseFailed)
			logger.Log("error", err, "phase", action)
			errs = append(errs, fmt.Errorf("dry-run upgrade failed: %w", err))
			break
		}
		if diff != "" {
			switch r.config.LogDiffs {
			case true:
				logger.Log("info", "difference detected during release comparison", "diff", diff, "phase", action)
			default:
				logger.Log("info", "difference detected during release comparison", "phase", action)
			}
			action = UpgradeAction
			goto next
		}
		if !status.HasRolledBack(hr) {
			status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseSucceeded)
		}
		logger.Log("info", "no changes", "phase", action)
	case InstallAction:
		logger.Log("info", "running installation", "phase", action)
		newRel, err = r.install(client, hr, chart, values)
		if err != nil {
			logger.Log("error", err, "phase", action)
			errs = append(errs, err)

			action = UninstallAction
			goto next
		}

		logger.Log("info", "installation succeeded", "revision", chart.revision, "phase", action)

		action = TestAction
		goto next
	case MigrateAction:
		logger.Log("info", "running 2to3 migration", "phase", action)
		var dryRun bool
		if hr.GetAnnotations()[MigrateAnnotation] == "true" {
			dryRun = false
		} else {
			dryRun = true
			logger.Log("info", "running helm 2to3 conversion in dry-run mode")
		}
		newRel, err = r.migrate(client, hr, chart, dryRun)

		if err != nil {
			status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseFailed)
			err = fmt.Errorf("failed to convert helm chart from v2 to v3: %w", err)
			logger.Log("error", err, "phase", action)
			errs = append(errs, err)
			break
		}

		// once migration is complete, we can go ahead and treat the HelmRelease as an upgrade since there might be changes in the chart spec
		action = UpgradeAction
		if dryRun {
			action = SkipAction
		}
		goto next
	case UpgradeAction:
		logger.Log("info", "running upgrade", "action", action)
		newRel, err = r.upgrade(client, hr, chart, values)

		if err != nil {
			logger.Log("error", err, "action", action)
			errs = append(errs, err)

			action = RollbackAction
			goto next
		}

		logger.Log("info", "upgrade succeeded", "revision", chart.revision, "phase", action)

		action = TestAction
		goto next
	case TestAction:
		if hr.Spec.Test.Enable {
			logger.Log("info", "running test", "action", TestAction)

			if err = r.test(client, hr); err != nil {
				logger.Log("error", err, "action", TestAction)
				errs = append(errs, err)

				if !hr.Spec.Test.GetIgnoreFailures() {
					if curRel == nil {
						action = UninstallAction
					} else {
						action = RollbackAction
					}
					goto next
				} else {
					logger.Log("info", "test failed - ignoring failures", "revision", chart.revision)
				}
			} else {
				logger.Log("info", "test succeeded", "revision", chart.revision, "action", action)
			}
		}

		status.SetStatusPhaseWithRevision(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseSucceeded, chart.revision)

		action = AnnotateAction
		goto next
	case AnnotateAction:
		if err := annotate(hr, newRel); err != nil {
			logger.Log("warning", err, "phase", action)
		}
	case RollbackAction:
		if hr.Spec.Rollback.Enable {
			latestRel, err := client.Get(hr.GetReleaseName(), helm.GetOptions{Namespace: hr.GetTargetNamespace(), Version: 0})
			if err != nil {
				err = fmt.Errorf("unable to determine if rollback should be performed: %w", err)
				logger.Log("error", err, "phase", action)
				errs = append(errs, err)
				break
			}
			if curRel.Version < latestRel.Version {
				logger.Log("info", "running rollback", "phase", action)
				if newRel, err = r.rollback(client, hr, chart.revision); err != nil {
					errs = append(errs, err)
					logger.Log("error", err, "phase", action)
					break
				}
				logger.Log("info", "rollback succeeded", "phase", action)

				action = AnnotateAction
				goto next
			}
		}
	case UninstallAction:
		logger.Log("info", "running uninstall", "phase", action)
		if err := uninstall(client, hr); err != nil {
			logger.Log("warning", err, "phase", action)
		}
		if hr.Spec.GitChartSource != nil {
			r.gitChartSync.Delete(hr)
		}
	}
	if errs.Empty() {
		return nil
	}
	return errs
}

// dryRunCompare performs a dry-run upgrade with the given
// `HelmRelease`, chart and values, and  makes a comparison with the
// given release. It returns the dry-run  release and a diff string,
// or an error.
func (r *Release) dryRunCompare(client helm.Client, rel *helm.Release, hr *apiV1.HelmRelease,
	chart chart, values []byte) (dryRel *helm.Release, diff string, err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, DryRunCompareAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	dryRel, err = client.UpgradeFromPath(chart.chartPath, hr.GetReleaseName(), values, helm.UpgradeOptions{
		DryRun:      true,
		Namespace:   hr.GetTargetNamespace(),
		Force:       hr.Spec.ForceUpgrade,
		ReuseValues: hr.GetReuseValues(),
		ResetValues: !hr.GetReuseValues(),
	})
	if err != nil {
		err = fmt.Errorf("dry-run upgrade for comparison failed: %w", err)
		return
	}
	diff = helm.Diff(rel, dryRel)
	return
}

const (
	AppIdLabelKey         = "oam.runtime.app.id"
	ComponentIdLabelKey   = "oam.runtime.component.id"
	IstioEnableLabelKey   = "istio-injection"
	IstioEnableLabelValue = "enabled"
	LogCollectAnnotateKey = "logCollect"
)

var (
	statefulsetGroupVersionResource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	deploymentGroupVersionResource  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	matchLabelsPath                 = []string{"spec", "selector", "matchLabels"}
	templateLabelsPath              = []string{"spec", "template", "metadata", "labels"}
)

type appManagerPostRenderer func(renderedManifests *bytes.Buffer) (modifiedManifests *bytes.Buffer, err error)

func (a appManagerPostRenderer) Run(renderedManifests *bytes.Buffer) (modifiedManifests *bytes.Buffer, err error) {
	return a(renderedManifests)
}

func (r *Release) appInfoInject(hr *apiV1.HelmRelease, target unstructured.Unstructured) unstructured.Unstructured {
	matchLabels, _, _ := unstructured.NestedStringMap(target.Object, matchLabelsPath...)
	if matchLabels == nil {
		matchLabels = make(map[string]string)
	}
	matchLabels[AppIdLabelKey] = hr.Spec.AppId
	matchLabels[ComponentIdLabelKey] = hr.Spec.ComponentId
	_ = unstructured.SetNestedStringMap(target.Object, matchLabels, matchLabelsPath...)

	templateLabels, _, _ := unstructured.NestedStringMap(target.Object, templateLabelsPath...)
	if templateLabels == nil {
		templateLabels = make(map[string]string)
	}
	templateLabels[AppIdLabelKey] = hr.Spec.AppId
	templateLabels[ComponentIdLabelKey] = hr.Spec.ComponentId
	_ = unstructured.SetNestedStringMap(target.Object, templateLabels, templateLabelsPath...)

	return target
}

func (r *Release) istioInject(hr *apiV1.HelmRelease, target unstructured.Unstructured) unstructured.Unstructured {

	matchLabels, _, _ := unstructured.NestedStringMap(target.Object, matchLabelsPath...)
	if matchLabels == nil {
		matchLabels = make(map[string]string)
	}
	matchLabels[IstioEnableLabelKey] = IstioEnableLabelValue
	_ = unstructured.SetNestedStringMap(target.Object, matchLabels, matchLabelsPath...)

	templateLabels, _, _ := unstructured.NestedStringMap(target.Object, templateLabelsPath...)
	if templateLabels == nil {
		templateLabels = make(map[string]string)
	}
	templateLabels[IstioEnableLabelKey] = IstioEnableLabelValue
	_ = unstructured.SetNestedStringMap(target.Object, templateLabels, templateLabelsPath...)

	return target
}

func (r *Release) deleteOldRes(client dynamic.Interface, resource schema.GroupVersionResource, namespace, name string) error {
	err := client.Resource(resource).Namespace(namespace).Delete(name, &metav1.DeleteOptions{})
	if err != nil {
		return err
	}

	//????????????????????????
	withTimeout, cancelFunc := context.WithTimeout(context.TODO(), 10*time.Second)
	wait.UntilWithContext(withTimeout, func(context.Context) {
		_, err := client.Resource(resource).Namespace(namespace).Get(name, metav1.GetOptions{})
		if err != nil && errors.IsNotFound(err) {
			cancelFunc()
		}
	}, time.Second)
	return nil
}

func (r *Release) istioInjectHandle(hr *apiV1.HelmRelease, client dynamic.Interface, resource schema.GroupVersionResource, target unstructured.Unstructured, istioInject bool) (unstructured.Unstructured, error) {
	current, err := client.Resource(resource).Namespace(hr.Namespace).Get(target.GetName(), metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return target, err
		} else {
			//????????????????????????istio??????
			if istioInject {
				target = r.istioInject(hr, target)
			}
			return target, nil
		}

	} else {
		templateLabels, _, _ := unstructured.NestedStringMap(current.Object, templateLabelsPath...)
		value, ok := templateLabels[IstioEnableLabelKey]
		//??????????????????
		if istioInject {
			//??????????????????????????????????????????istio??????????????????????????????????????????????????????
			if !ok || value != IstioEnableLabelValue {
				err := r.deleteOldRes(client, resource, target.GetNamespace(), target.GetName())
				if err != nil {
					return target, err
				}
			}
			return r.istioInject(hr, target), nil
		} else {
			//???????????????????????????????????????????????????????????????istio??????????????????????????????????????????????????????
			if ok && value == IstioEnableLabelValue {
				err := r.deleteOldRes(client, resource, target.GetNamespace(), target.GetName())
				if err != nil {
					return target, err
				}
			}
			return target, nil
		}
	}
}

func (r *Release) getAppManagerPostRenderer(hr *apiV1.HelmRelease) postrender.PostRenderer {
	return appManagerPostRenderer(func(renderedManifests *bytes.Buffer) (modifiedManifests *bytes.Buffer, err error) {
		config, err := clientcmd.BuildConfigFromFlags("", "")
		if err != nil {
			klog.Error(err.Error())
			return renderedManifests, nil
		}

		dynamicClient, err := dynamic.NewForConfig(config)
		if err != nil {
			klog.Error(err.Error())
			return renderedManifests, nil
		}

		helmReleaseSpec := hr.Spec
		unstructuredList := releaseManifestToUnstructured(renderedManifests.String())
		modifiedManifests = bytes.NewBuffer([]byte{})
		for _, u := range unstructuredList {

			labels := u.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}

			labels[AppIdLabelKey] = helmReleaseSpec.AppId
			labels[ComponentIdLabelKey] = helmReleaseSpec.ComponentId
			u.SetLabels(labels)

			switch u.GetKind() {
			case "StatefulSet", "Deployment":
				annotations := u.GetAnnotations()
				if annotations == nil {
					annotations = make(map[string]string)
				}

				annotations[LogCollectAnnotateKey] = strconv.FormatBool(helmReleaseSpec.LogCollect)
				u.SetAnnotations(annotations)
			}

			switch u.GetKind() {
			case "StatefulSet":
				u = r.appInfoInject(hr, u)
				istioInjectHandled, err := r.istioInjectHandle(hr, dynamicClient, statefulsetGroupVersionResource, u, helmReleaseSpec.IstioEnabled)
				if err != nil {
					klog.Error(err.Error())
				}
				u = istioInjectHandled
			case "Deployment":
				u = r.appInfoInject(hr, u)
				istioInjectHandled, err := r.istioInjectHandle(hr, dynamicClient, deploymentGroupVersionResource, u, helmReleaseSpec.IstioEnabled)
				if err != nil {
					klog.Error(err.Error())
				}
				u = istioInjectHandled
			}

			modifiedManifests.WriteString("---\n")
			marshal, _ := yaml.Marshal(u.Object)
			modifiedManifests.Write(marshal)
			modifiedManifests.WriteString("\n")
		}
		klog.Info(modifiedManifests.String())
		return modifiedManifests, nil
	})

}

// install performs an installation with the given HelmRelease,
// chart, and values while recording the phases on the HelmRelease.
// It returns the release result or an error.
func (r *Release) install(client helm.Client, hr *apiV1.HelmRelease, chart chart, values []byte) (rel *helm.Release, err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, InstallAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	status.SetStatusPhaseWithRevision(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseInstalling, chart.revision)
	rel, err = client.UpgradeFromPath(chart.chartPath, hr.GetReleaseName(), values, helm.UpgradeOptions{
		Namespace:         hr.GetTargetNamespace(),
		Timeout:           hr.GetTimeout(),
		Install:           true,
		Force:             hr.Spec.ForceUpgrade,
		SkipCRDs:          hr.Spec.SkipCRDs,
		MaxHistory:        hr.GetMaxHistory(),
		Wait:              hr.GetWait(),
		DisableValidation: hr.Spec.DisableOpenAPIValidation,
		PostRenderer:      r.getAppManagerPostRenderer(hr),
	})
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseDeployFailed)
		err = fmt.Errorf("installation failed: %w", err)
		return
	}
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseDeployed)
	return
}

// migrate performs a migration with the given HelmRelease,
// chart, and values while recording the phases on the HelmRelease.
// It returns the release result or an error.
func (r *Release) migrate(client helm.Client, hr *apiV1.HelmRelease, chart chart, dryRun bool) (rel *helm.Release, err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, MigrateAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	status.SetStatusPhaseWithRevision(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseMigrating, chart.revision)

	err = r.converter.Convert(hr.GetReleaseName(), dryRun)
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseFailed)
		err = fmt.Errorf("installation failed: %w", err)
		return
	}
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseSucceeded)
	return
}

// upgrade performs an upgrade with the given HelmRelease,
// chart and values while recording the phases and revision on
// the HelmRelease. It returns the release result or an error.
func (r *Release) upgrade(client helm.Client, hr *apiV1.HelmRelease, chart chart, values []byte) (rel *helm.Release, err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, UpgradeAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	status.SetStatusPhaseWithRevision(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseUpgrading, chart.revision)
	rel, err = client.UpgradeFromPath(chart.chartPath, hr.GetReleaseName(), values, helm.UpgradeOptions{
		Namespace:         hr.GetTargetNamespace(),
		Timeout:           hr.GetTimeout(),
		Install:           false,
		Force:             hr.Spec.ForceUpgrade,
		ReuseValues:       hr.GetReuseValues(),
		ResetValues:       !hr.GetReuseValues(),
		SkipCRDs:          hr.Spec.SkipCRDs,
		MaxHistory:        hr.GetMaxHistory(),
		Wait:              hr.GetWait(),
		DisableValidation: hr.Spec.DisableOpenAPIValidation,
		PostRenderer:      r.getAppManagerPostRenderer(hr),
	})
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseDeployFailed)
		err = fmt.Errorf("upgrade failed: %w", err)
		return
	}
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseDeployed)
	return
}

// rollback performs a rollback for the given HelmRelease,
// while recording the phases on  the HelmRelease. It returns
// the release result or an error.
func (r *Release) rollback(client helm.Client, hr *apiV1.HelmRelease, revision string) (rel *helm.Release, err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, RollbackAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())

	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseRollingBack)
	rel, err = client.Rollback(hr.GetReleaseName(), helm.RollbackOptions{
		Namespace:    hr.GetTargetNamespace(),
		Timeout:      hr.Spec.Rollback.GetTimeout(),
		Wait:         hr.Spec.Rollback.Wait,
		DisableHooks: hr.Spec.Rollback.DisableHooks,
		Recreate:     hr.Spec.Rollback.Recreate,
		Force:        hr.Spec.Rollback.Force,
	})
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseRollbackFailed)
		err = fmt.Errorf("rollback failed: %w", err)
		return
	}
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseRolledBack)
	return
}

// test performs a test for the given HelmRelease,
// while recording the phases on  the HelmRelease. It returns
// the release result or an error.
func (r *Release) test(client helm.Client, hr *apiV1.HelmRelease) (err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, TestAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseTesting)
	err = client.Test(hr.GetReleaseName(), helm.TestOptions{
		Namespace: hr.GetTargetNamespace(),
		Timeout:   hr.Spec.Test.GetTimeout(),
		Cleanup:   hr.Spec.Test.GetCleanup(),
	})
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseTestFailed)
		err = fmt.Errorf("test failed: %w", err)
		return
	}
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, apiV1.HelmReleasePhaseTested)
	return
}

// annotate annotates the given release resources on the cluster with
// the resource ID of the given HelmRelease.
func annotate(hr *apiV1.HelmRelease, rel *helm.Release) (err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, AnnotateAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	err = annotateResources(rel, hr.ResourceID())
	if err != nil {
		err = fmt.Errorf("failed to annotate release resources: %w", err)
	}
	return
}

func uninstall(client helm.Client, hr *apiV1.HelmRelease) (err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, UninstallAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	err = client.Uninstall(hr.GetReleaseName(), helm.UninstallOptions{
		Namespace:   hr.GetTargetNamespace(),
		KeepHistory: false,
		Timeout:     hr.GetTimeout(),
	})
	if err != nil {
		err = fmt.Errorf("uninstall failed: %w", err)
	}
	return
}

// releaseLogger returns a logger in the context of the given
// HelmRelease (that being, with metadata included).
func releaseLogger(logger log.Logger, client helm.Client, hr *apiV1.HelmRelease) log.Logger {
	return log.With(logger,
		"release", hr.GetReleaseName(),
		"targetNamespace", hr.GetTargetNamespace(),
		"resource", hr.ResourceID().String(),
		"helmVersion", client.Version(),
	)
}
