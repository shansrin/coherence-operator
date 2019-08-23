package main

import (
	"context"
	"fmt"
	"github.com/oracle/coherence-operator/test/e2e/helper"
	"github.com/pborman/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/helm/pkg/chartutil"
	helmengine "k8s.io/helm/pkg/engine"
	"k8s.io/helm/pkg/kube"
	cpb "k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/proto/hapi/services"
	"k8s.io/helm/pkg/storage"
	"k8s.io/helm/pkg/storage/driver"
	"k8s.io/helm/pkg/tiller"
	"k8s.io/helm/pkg/tiller/environment"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/operator-framework/operator-sdk/pkg/helm/client"
	"github.com/operator-framework/operator-sdk/pkg/helm/engine"
)

// This method is used by the Operator build to generate a yaml manifest that
// is used by the Operator SDK test framework to deploy an Operator. The manifest
// is generated by using the Helm API to run a Helm install of the Operator Helm
// chart with dry-run and debug enabled then capturing the yaml that the install
// would have produced.
func main() {
	namespace := helper.GetTestNamespace()
	cfg, _, err := helper.GetKubeconfigAndNamespace("")
	clientv1, err := v1.NewForConfig(cfg)
	if err != nil {
		panic(err)
	}

	storageBackend := storage.Init(driver.NewSecrets(clientv1.Secrets(namespace)))

	mgr, err := manager.New(cfg, manager.Options{
		Namespace:      namespace,
		MapperProvider: apiutil.NewDiscoveryRESTMapper,
		LeaderElection: false,
	})

	tillerKubeClient, err := client.NewFromManager(mgr)
	if err != nil {
		panic(err)
	}

	chartDir, err := helper.FindOperatorHelmChartDir()
	if err != nil {
		panic(err)
	}

	values := helper.OperatorValues{}
	vf := helper.GetTestManifestValuesFileName()
	if vf != "" {
		err = values.LoadFromYaml(vf)
		if err != nil {
			panic(err)
		}
	} else {
		err = values.LoadFromYaml(chartDir + string(os.PathSeparator) + "values.yaml")
		if err != nil {
			panic(err)
		}
	}

	cr, err := values.ToYaml()
	if err != nil {
		panic(err)
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "coherence.oracle.com", Version: "v1", Kind: "Operator"})
	u.SetNamespace(namespace)

	uid := uuid.Parse(uuid.New()).String()
	u.SetUID(types.UID(uid))
	u.SetName("test")

	releaseServer, err := getReleaseServer(u, storageBackend, tillerKubeClient)
	if err != nil {
		panic(err)
	}

	chart, err := loadChart(chartDir)
	if err != nil {
		panic(err)
	}

	chart.Dependencies = make([]*cpb.Chart, 0)

	config := &cpb.Config{Raw: string(cr)}

	dryRunReq := &services.InstallReleaseRequest{
		Name:         "operator",
		Chart:        chart,
		Values:       config,
		DryRun:       true,
		Namespace:    namespace,
		DisableHooks: true,
	}

	dryRunResponse, err := releaseServer.InstallRelease(context.TODO(), dryRunReq)
	if err != nil {
		panic(err)
	}

	fName, err := helper.GetTestManifestFileName()
	if err != nil {
		panic(err)
	}

	f, err := os.Create(fName)
	if err != nil {
		panic(err)
	}

	_, err = f.WriteString(dryRunResponse.Release.Manifest)
	if err != nil {
		panic(err)
	}
	_ = f.Close()
}

// getReleaseServer creates a ReleaseServer configured with a rendering engine that adds ownerrefs to rendered assets
// based on the CR.
func getReleaseServer(cr *unstructured.Unstructured, storageBackend *storage.Storage, tillerKubeClient *kube.Client) (*tiller.ReleaseServer, error) {
	controllerRef := metav1.NewControllerRef(cr, cr.GroupVersionKind())
	ownerRefs := []metav1.OwnerReference{
		*controllerRef,
	}
	baseEngine := helmengine.New()
	e := engine.NewOwnerRefEngine(baseEngine, ownerRefs)
	var ey environment.EngineYard = map[string]environment.Engine{
		environment.GoTplEngine: e,
	}
	env := &environment.Environment{
		EngineYard: ey,
		Releases:   storageBackend,
		KubeClient: tillerKubeClient,
	}
	kubeconfig, err := tillerKubeClient.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := clientset.NewForConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	return tiller.NewReleaseServer(env, cs, false), nil
}

func loadChart(chartDir string) (*cpb.Chart, error) {
	// chart is mutated by the call to processRequirements,
	// so we need to reload it from disk every time.
	chart, err := chartutil.LoadDir(chartDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %s", err)
	}

	return chart, nil
}
