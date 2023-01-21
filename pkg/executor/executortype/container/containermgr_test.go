package container

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	uuid "github.com/satori/go.uuid"
	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	k8sCache "k8s.io/client-go/tools/cache"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	fClient "github.com/fission/fission/pkg/generated/clientset/versioned/fake"
	genInformer "github.com/fission/fission/pkg/generated/informers/externalversions"
	"github.com/fission/fission/pkg/utils"
	"github.com/fission/fission/pkg/utils/loggerfactory"
)

const (
	defaultNamespace  string = "default"
	functionNamespace string = "fission-function"
	builderNamespace  string = "fission-builder"
	functionName      string = "container-test-func"
	configmapName     string = "container-test-configmap"
)

func TestRefreshFuncPods(t *testing.T) {
	os.Setenv("DEBUG_ENV", "true")
	logger := loggerfactory.GetLogger()
	kubernetesClient := fake.NewSimpleClientset()
	fissionClient := fClient.NewSimpleClientset()
	factory := make(map[string]genInformer.SharedInformerFactory, 0)
	factory[metav1.NamespaceAll] = genInformer.NewSharedInformerFactory(fissionClient, time.Minute*30)

	executorLabel, err := utils.GetInformerLabelByExecutor(fv1.ExecutorTypeContainer)
	if err != nil {
		t.Fatalf("Error creating labels for informer: %s", err)
	}
	cmInformerFactory := utils.GetInformerFactoryByExecutor(kubernetesClient, executorLabel, time.Minute*30)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	executor, err := MakeContainer(ctx, logger, fissionClient, kubernetesClient, "test",
		factory, cmInformerFactory)
	if err != nil {
		t.Fatalf("container manager creation failed: %s", err)
	}

	ncm := executor.(*Container)

	nsResolver := utils.NamespaceResolver{
		FunctionNamespace: functionNamespace,
		BuiderNamespace:   builderNamespace,
		DefaultNamespace:  defaultNamespace,
	}
	ncm.nsResolver = &nsResolver

	go ncm.Run(ctx)
	t.Log("Container manager started")

	for _, f := range factory {
		f.Start(ctx.Done())
	}
	for _, informerFactory := range cmInformerFactory {
		informerFactory.Start(ctx.Done())
	}

	t.Log("Informers required for container manager started")

	waitSynced := make([]k8sCache.InformerSynced, 0)
	for _, deplListerSynced := range ncm.deplListerSynced {
		waitSynced = append(waitSynced, deplListerSynced)
	}
	for _, svcListerSynced := range ncm.svcListerSynced {
		waitSynced = append(waitSynced, svcListerSynced)
	}

	if ok := k8sCache.WaitForCacheSync(ctx.Done(), waitSynced...); !ok {
		t.Fatal("Timed out waiting for caches to sync")
	}

	funcUID, err := uuid.NewV4()
	if err != nil {
		t.Fatal(err)
	}
	funcSpec := fv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      functionName,
			Namespace: defaultNamespace,
			UID:       types.UID(funcUID.String()),
		},
		Spec: fv1.FunctionSpec{
			Environment: fv1.EnvironmentReference{
				Name:      "",
				Namespace: "",
			},
			InvokeStrategy: fv1.InvokeStrategy{
				ExecutionStrategy: fv1.ExecutionStrategy{
					ExecutorType: fv1.ExecutorTypeContainer,
				},
			},
		},
	}
	_, err = fissionClient.CoreV1().Functions(defaultNamespace).Create(ctx, &funcSpec, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating function failed : %s", err)
	}

	funcRes, err := fissionClient.CoreV1().Functions(defaultNamespace).Get(ctx, functionName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error getting function: %s", err)
	}
	assert.Equal(t, funcRes.ObjectMeta.Name, functionName)

	ctx2, cancel2 := context.WithCancel(context.Background())

	wait.Until(func() {
		t.Log("Checking for deployment")
		ret, err := kubernetesClient.AppsV1().Deployments(functionNamespace).List(ctx2, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("Error getting deployment: %s", err)
		}
		if len(ret.Items) > 0 {
			t.Log("Deployment created", ret.Items[0].Name)
			cancel2()
		}
	}, time.Second*2, ctx2.Done())

	err = BuildConfigMap(ctx, kubernetesClient, defaultNamespace, configmapName, map[string]string{
		"test-key": "test-value",
	})
	if err != nil {
		t.Fatalf("Error building configmap: %s", err)
	}

	t.Log("Adding configmap to function")
	funcRes.Spec.ConfigMaps = []fv1.ConfigMapReference{
		{
			Name:      configmapName,
			Namespace: defaultNamespace,
		},
	}
	_, err = fissionClient.CoreV1().Functions(defaultNamespace).Update(ctx, funcRes, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Error updating function: %s", err)
	}
	funcRes, err = fissionClient.CoreV1().Functions(defaultNamespace).Get(ctx, functionName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error getting function: %s", err)
	}
	assert.Greater(t, len(funcRes.Spec.ConfigMaps), 0)

	err = ncm.RefreshFuncPods(ctx, logger, *funcRes)
	if err != nil {
		t.Fatalf("Error refreshing function pods: %s", err)
	}

	funcLabels := ncm.getDeployLabels(funcRes.ObjectMeta)

	dep, err := kubernetesClient.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(funcLabels).AsSelector().String(),
	})

	if err != nil {
		t.Fatalf("Error getting deployment: %s", err)
	}
	assert.Equal(t, len(dep.Items), 1)

	cm, err := kubernetesClient.CoreV1().ConfigMaps(defaultNamespace).Get(ctx, configmapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error getting configmap: %s", err)
	}
	assert.Equal(t, cm.ObjectMeta.Name, configmapName)
}

func FakeResourceVersion() string {
	return fmt.Sprint(time.Now().Nanosecond())[:6]
}

func BuildConfigMap(ctx context.Context, kubernetesClient *fake.Clientset, namespace, name string, data map[string]string) error {
	testConfigMap := apiv1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: FakeResourceVersion(),
		},
		Data: data,
	}
	_, err := kubernetesClient.CoreV1().ConfigMaps(namespace).Create(ctx, &testConfigMap, metav1.CreateOptions{})
	return err
}
