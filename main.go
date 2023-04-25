package main

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	kcv1alpha1 "github.com/vmware-tanzu/carvel-kapp-controller/pkg/apis/kappctrl/v1alpha1"
	"github.com/vmware-tanzu/carvel-kapp-controller/pkg/client/clientset/versioned"
	kcexternalversions "github.com/vmware-tanzu/carvel-kapp-controller/pkg/client/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var appList = []string{"accelerator", "api-auto-registration", "api-portal", "appliveview", "appliveview-apiserver", "appliveview-connector", "appliveview-conventions",
	"appsso", "bitnami-services", "buildservice", "cartographer", "cert-manager", "cnrs", "contour", "crossplane", "developer-conventions", "eventing",
	"fluxcd-source-controller", "grype", "learningcenter", "learningcenter-workshops", "metadata-store", "namespace-provisioner", "ootb-delivery-basic",
	"ootb-supply-chain-basic", "ootb-templates", "policy-controller", "scanning", "service-bindings", "services-toolkit", "source-controller", "spring-boot-conventions",
	"tap", "tap-auth", "tap-gui", "tap-telemetry", "tekton-pipelines"}

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

type handler struct {
	stopperChan chan struct{}
	watchError  error
}

type AppStatusDiff struct {
	old kcv1alpha1.AppStatus
	new kcv1alpha1.AppStatus
}

func main() {

	fmt.Printf("\n**** Collecting apps installation time... ***\n")
	config, err := clientcmd.BuildConfigFromFlags("", "/Users/ktanushree/.kube/config") // need to change it to take from user
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
	writeIntoCSV()
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
	oldApp, _ := oldObj.(*kcv1alpha1.App)

	installationDetail.lock.Lock()
	defer installationDetail.lock.Unlock()
	if _, found := installationDetail.detail[newApp.Name]; !found {
		if newApp.Status.ConsecutiveReconcileSuccesses == 1 {
			installationDetail.detail[newApp.Name] = appDetails{
				name:          newApp.Name,
				namespace:     newApp.Namespace,
				creationTime:  newApp.CreationTimestamp.UTC().String(),
				fetchTime:     newApp.Status.Fetch.StartedAt.UTC().String(),
				collectedTime: time.Now().UTC().String(),
			}

		}
	}

	appStatus := AppStatusDiff{
		old: oldApp.Status,
		new: newApp.Status,
	}

	stopWatch, err := appStatus.CheckAppStatus()
	if err != nil {
		fmt.Errorf("Error: %s\n", err.Error())
	}

	if stopWatch {
		h.stopWatch()
	}
}

func (h *handler) stopWatch() {
	close(h.stopperChan)
}

// Check apps status
func (d *AppStatusDiff) CheckAppStatus() (bool, error) {
	if d.new.Fetch != nil {
		if d.old.Fetch == nil || !d.old.Fetch.UpdatedAt.Equal(&d.new.Fetch.UpdatedAt) {
			if d.new.Fetch.ExitCode != 0 && d.new.Fetch.UpdatedAt.Unix() >= d.new.Fetch.StartedAt.Unix() {
				msg := "Fetch failed"
				return true, fmt.Errorf(msg)
			}
		}
	}
	if d.new.Template != nil {
		if d.old.Template == nil || !d.old.Template.UpdatedAt.Equal(&d.new.Template.UpdatedAt) {
			if d.new.Template.ExitCode != 0 {
				msg := "Template failed"
				return true, fmt.Errorf(msg)
			}
		}
	}
	if d.new.Deploy != nil {
		isDeleting := IsDeleting(d.new)
		ongoingOp := "Deploy"
		if isDeleting {
			ongoingOp = "Delete"
		}

		if d.old.Deploy == nil || !d.old.Deploy.UpdatedAt.Equal(&d.new.Deploy.UpdatedAt) {
			if d.new.Deploy.ExitCode != 0 && d.new.Deploy.Finished {
				msg := fmt.Sprintf("%s failed", ongoingOp)
				return true, fmt.Errorf(msg)
			}
		}
	}

	if HasReconciled(d.new) {
		return true, nil
	}
	failed, errMsg := HasFailed(d.new)
	if failed {
		return true, fmt.Errorf(errMsg)
	}
	return false, nil
}

func HasReconciled(status kcv1alpha1.AppStatus) bool {
	for _, condition := range status.Conditions {
		if condition.Type == kcv1alpha1.ReconcileSucceeded && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func HasFailed(status kcv1alpha1.AppStatus) (bool, string) {
	for _, condition := range status.Conditions {
		if condition.Type == kcv1alpha1.ReconcileFailed && condition.Status == corev1.ConditionTrue {
			return true, fmt.Sprintf("%s: %s", kcv1alpha1.ReconcileFailed, status.UsefulErrorMessage)
		}
		if condition.Type == kcv1alpha1.DeleteFailed && condition.Status == corev1.ConditionTrue {
			return true, fmt.Sprintf("%s: %s", kcv1alpha1.DeleteFailed, status.UsefulErrorMessage)
		}
	}
	return false, ""
}

func IsDeleting(status kcv1alpha1.AppStatus) bool {
	for _, condition := range status.Conditions {
		if condition.Type == kcv1alpha1.Deleting && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func writeIntoCSV() {
	file, err := os.Create("records.csv") // later we can change it to take user input
	defer file.Close()
	if err != nil {
		log.Fatalln("failed to open file", err)
	}
	w := csv.NewWriter(file)
	defer w.Flush()

	row := []string{"AppName", "CreationTimestamp", "FetchStartedAt", "DataCollectedAt"}
	if err := w.Write(row); err != nil {
		log.Fatalln("error writing record to file", err)
	}

	installationDetail.lock.Lock()
	defer installationDetail.lock.Unlock()
	for _, record := range installationDetail.detail {
		row := []string{record.name, record.creationTime, record.fetchTime, record.collectedTime}
		if err := w.Write(row); err != nil {
			log.Fatalln("error writing record to file", err)
		}
	}

}
