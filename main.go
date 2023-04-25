package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	cmdcore "github.com/vmware-tanzu/carvel-kapp-controller/cli/pkg/kctrl/cmd/core"
	kcv1alpha1 "github.com/vmware-tanzu/carvel-kapp-controller/pkg/apis/kappctrl/v1alpha1"
	"github.com/vmware-tanzu/carvel-kapp-controller/pkg/client/clientset/versioned"
	kcexternalversions "github.com/vmware-tanzu/carvel-kapp-controller/pkg/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var appList = []string{"accelerator", "api-auto-registration", "api-portal", "appliveview", "appliveview-apiserver", "appliveview-connector", "appliveview-conventions",
	"appsso", "bitnami-services", "buildservice", "cartographer", "cert-manager", "cnrs", "contour", "crossplane", "developer-conventions", "eventing",
	"fluxcd-source-controller", "grype", "learningcenter", "learningcenter-workshops", "metadata-store", "namespace-provisioner", "ootb-delivery-basic",
	"ootb-supply-chain-basic", "ootb-templates", "policy-controller", "scanning", "service-bindings", "services-toolkit", "source-controller", "spring-boot-conventions",
	"tap", "tap-auth", "tap-gui"}

type appDetails struct {
	name          string
	namespace     string
	creationTime  string
	fetchTime     string
	collectedTime string
}

type result struct {
	detail map[string]appDetails // key will be app name
	lock   sync.Mutex
}

var installationDetail = result{
	detail: make(map[string]appDetails),
}

func main() {

	fmt.Printf("\n**** start script ***\n")
	config, err := clientcmd.BuildConfigFromFlags("", "/Users/ktanushree/.kube/config")
	if err != nil {
		log.Fatal(err)
	}

	// create the clientset
	clientset, err := versioned.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	wg := new(sync.WaitGroup)

	for _, appName := range appList {
		wg.Add(1)
		go sharedInformer(clientset, appName, wg)
	}
	wg.Wait()
	// installationDetail.lock.Lock()
	// fmt.Printf("\nResult: %+v\n", installationDetail.detail)
	// installationDetail.lock.Unlock()
}

type handler struct {
	stopperChan          chan struct{}
	watchError           error
	lastSeenDeployStdout string
	statusUI             cmdcore.StatusLoggingUI
}

func sharedInformer(clientset *versioned.Clientset, appName string, wg *sync.WaitGroup) {

	defer wg.Done()
	stopperChan := make(chan struct{})
	shareInformer := kcexternalversions.NewFilteredSharedInformerFactory(clientset, 10*time.Second, "tap-install", func(opts *metav1.ListOptions) {
		opts.FieldSelector = fmt.Sprintf("metadata.name=%s", appName)
	})

	h := handler{
		stopperChan: stopperChan,
	}

	informer := shareInformer.Kappctrl().V1alpha1().Apps().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: h.udpateEventHandler,
	})

	//fmt.Printf("\n**** start go routine ***\n")
	go informer.Run(h.stopperChan)

	if !cache.WaitForCacheSync(stopperChan, informer.HasSynced) {
		fmt.Errorf("Timed out waiting for caches to sync")
		return
	}

	<-h.stopperChan
	if h.watchError != nil {
		fmt.Errorf("Reconciling app: %s", h.watchError)
		return
	}
}

func (h *handler) udpateEventHandler(oldObj interface{}, newObj interface{}) {

	newApp, _ := newObj.(*kcv1alpha1.App)
	//oldApp, _ := newObj.(*kcv1alpha1.App)
	//fmt.Printf("\n\nNew app creation time: [%+v]\n\n", newApp.CreationTimestamp.Time)

	installationDetail.lock.Lock()
	if _, found := installationDetail.detail[newApp.Name]; !found {
		if newApp.Status.ConsecutiveReconcileSuccesses == 1 {
			installationDetail.detail[newApp.Name] = appDetails{
				name:          newApp.Name,
				namespace:     newApp.Namespace,
				creationTime:  newApp.CreationTimestamp.String(),
				fetchTime:     newApp.Status.Fetch.StartedAt.String(),
				collectedTime: time.Now().Local().String(),
			}

		}
	}
	fmt.Printf("\nResult: %+v\n", installationDetail.detail)
	installationDetail.lock.Unlock()

	// stopWatch, deployOutput, err := app.NewAppStatusDiff(oldApp.Status, newApp.Status, h.statusUI, h.lastSeenDeployStdout).PrintUpdate()
	// h.lastSeenDeployStdout = deployOutput
	// h.watchError = err
	// if stopWatch {
	// 	h.stopWatch()
	// }
	h.stopWatch()
}

func (h *handler) stopWatch() {
	close(h.stopperChan)
}
