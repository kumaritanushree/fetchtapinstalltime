package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	kcv1alpha1 "github.com/vmware-tanzu/carvel-kapp-controller/pkg/apis/kappctrl/v1alpha1"
	"github.com/vmware-tanzu/carvel-kapp-controller/pkg/client/clientset/versioned"
	kcexternalversions "github.com/vmware-tanzu/carvel-kapp-controller/pkg/client/informers/externalversions"
	yaml "gopkg.in/yaml.v3"
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
	creationTime  metav1.Time
	fetchTime     metav1.Time
	collectedTime metav1.Time
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

var appNamespaces = make(map[string][]string) // map of app and namespaces used by that app

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
	writeInstallTimeIntoCSV()
	// printMetrics()

	// fetch cpu-memory usage by TAP components
	for app, ns_list := range appNamespaces {
		fmt.Printf("\nAppname: %s,  Namespaces: %+v\n", app, ns_list)
		fetchTapAppCPUMemUsage(ns_list, app)
	}
	writeCPUAndMemUsageIntoCSV()
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
				creationTime:  newApp.CreationTimestamp,
				fetchTime:     newApp.Status.Fetch.StartedAt,
				collectedTime: metav1.Now(),
			}
			appNamespaces[newApp.Name] = append(appNamespaces[newApp.Name], newApp.Status.Deploy.KappDeployStatus.AssociatedResources.Namespaces...)
		}
	}

	appStatus := AppStatusDiff{
		old: oldApp.Status,
		new: newApp.Status,
	}

	stopWatch, err := appStatus.CheckAppStatus(newApp.Name)
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
func (d *AppStatusDiff) CheckAppStatus(app string) (bool, error) {
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

func writeInstallTimeIntoCSV() {
	file, err := os.Create("install_time.csv") // later we can change it to take user input
	defer file.Close()
	if err != nil {
		log.Fatalln("failed to open file", err)
	}
	w := csv.NewWriter(file)
	defer w.Flush()

	row := []string{"AppName", "CreationTimestamp", "FetchStartedAt", "DataCollectedAt", "completion-start", "completion-fetch"}
	if err := w.Write(row); err != nil {
		log.Fatalln("error writing record to file", err)
	}

	installationDetail.lock.Lock()
	defer installationDetail.lock.Unlock()
	for _, record := range installationDetail.detail {
		row := []string{record.name, record.creationTime.UTC().String(), record.fetchTime.UTC().String(), record.collectedTime.UTC().String(), installTimeTaken(record.creationTime, record.collectedTime), installTimeTaken(record.fetchTime, record.collectedTime)}
		if err := w.Write(row); err != nil {
			log.Fatalln("error writing record to file", err)
		}
	}

}

func installTimeTaken(startTime, endTime metav1.Time) string {

	installTime := int((endTime.Time).Sub(startTime.Time).Seconds())

	return fmt.Sprintf("%dm %ds", installTime/60, installTime%60)
}

// *******************************************************************

type Container struct {
	Name  string `yaml:"name"`
	Usage struct {
		Cpu    string `yaml:"cpu"`
		Memory string `yaml:"memory"`
	} `yaml:"usage"`
}

type ContainerResource struct {
	Limits struct {
		Cpu    string `yaml:"cpu"`
		Memory string `yaml:"memory"`
	} `yaml:"limits"`
	Requests struct {
		Cpu    string `yaml:"cpu"`
		Memory string `yaml:"memory"`
	} `yaml:"requests"`
}

type PodMetricYml struct {
	Containers []Container `yaml:"containers"`
}

type Args struct {
	Name     string            `yaml:"name"`
	Resource ContainerResource `yaml:"resources"`
}
type PodYml struct {
	Spec struct {
		Containers []Args `yaml:"containers"`
	} `yaml:"spec"`
}

type ContainerMetric struct {
	Container
	CPUReq   string
	MemReq   string
	CPULimit string
	MemLimit string
}

type PodMetric struct {
	AppName     string
	PodName     string
	Namespace   string
	Containers  []ContainerMetric
	TotalCPU    string
	TotalMemory string
}

type AppCPUMemUsage struct {
	PodsMetric map[string][]PodMetric // appName will be key
	Lock       sync.Mutex
}

var AppMetric = AppCPUMemUsage{
	PodsMetric: make(map[string][]PodMetric),
}

func runCmd(args []string) (error, string) {

	var (
		stderr, stdout bytes.Buffer
	)

	cmd := exec.Command("kubectl", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("Error in run cmd: %s\n", stderr.String()), ""
	}

	return nil, stdout.String()
}

func fetchTapAppCPUMemUsage(namespaces []string, appName string) {

	wg := new(sync.WaitGroup)

	for _, ns := range namespaces {
		if ns == "kube-system" { // few apps are creating clusterRole, roleBinding kind of resources in this namespace
			continue
		}
		wg.Add(1)
		go pod_list(ns, appName, wg)
	}

	wg.Wait()
}

func pod_list(ns, appName string, wg *sync.WaitGroup) {

	defer wg.Done()
	var (
		err error
		out string
	)

	wg_pod := new(sync.WaitGroup)

	// List all pods in namespace
	err, out = runCmd([]string{"get", "pods", "-n", ns, "--no-headers", "-o", "custom-columns=:metadata.name"})
	if err != nil {
		fmt.Errorf("Error-pod: %s\n", err.Error())
		return
	}

	pod_list := strings.Split(out, "\n")
	fmt.Printf("\nNamespace: %s, pod list: %+v\n", ns, pod_list)
	for _, pod := range pod_list {
		if pod == "" {
			continue
		}
		wg_pod.Add(1)
		go fetch_cpu_mem_usage(pod, ns, appName, wg_pod)

	}
	wg_pod.Wait()
}

func fetch_cpu_mem_usage(pod, ns, appName string, wg *sync.WaitGroup) {

	defer wg.Done()

	// Fetch podMetrics for pods, it will give metrics per container in given pod
	err, out := runCmd([]string{"get", "PodMetrics", pod, "-n", ns, "-oyaml"})
	if err != nil {
		fmt.Errorf("Error-data-fetch: %s\n", err.Error())
		return
	}

	metricYml := PodMetricYml{}

	err = yaml.Unmarshal([]byte(out), &metricYml)
	if err != nil {
		fmt.Errorf("Unmarshal error: %s\n", err.Error())
		return
	}

	// Fetch podMetric of pod, it will give metric per pod
	err, out = runCmd([]string{"get", "PodMetrics", pod, "-n", ns, "--no-headers"})
	if err != nil {
		fmt.Errorf("Error-data-fetch: %s\n", err.Error())
		return
	}
	usageByPod := strings.Split(out, "  ")

	err, out = runCmd([]string{"get", "pod", pod, "-n", ns, "-oyaml"})
	if err != nil {
		fmt.Errorf("Error-data-fetch: %s\n", err.Error())
		return
	}

	conRes := PodYml{}
	err = yaml.Unmarshal([]byte(out), &conRes)
	if err != nil {
		fmt.Errorf("Unmarshal error: %s\n", err.Error())
		return
	}

	cMap := make(map[string]ContainerResource)
	for _, v := range conRes.Spec.Containers {
		cMap[v.Name] = v.Resource
	}

	contMet := []ContainerMetric{}
	for _, container := range metricYml.Containers {

		v := cMap[container.Name]
		contMet = append(contMet, ContainerMetric{
			Container: container,
			CPUReq:    v.Requests.Cpu,
			MemReq:    v.Requests.Memory,
			CPULimit:  v.Limits.Cpu,
			MemLimit:  v.Limits.Memory,
		})
	}

	temp := PodMetric{
		AppName:     appName,
		PodName:     pod,
		Namespace:   ns,
		Containers:  contMet,
		TotalCPU:    usageByPod[1],
		TotalMemory: usageByPod[2],
	}

	writeToMap(&temp, appName)
}

func writeToMap(tmp *PodMetric, appName string) {
	AppMetric.Lock.Lock()
	defer AppMetric.Lock.Unlock()
	AppMetric.PodsMetric[appName] = append(AppMetric.PodsMetric[appName], *tmp)
	fmt.Printf("\nApp: %s,  Pod metric lis: %+v\n", appName, AppMetric.PodsMetric[appName])
}

func printMetrics() {
	AppMetric.Lock.Lock()
	defer AppMetric.Lock.Unlock()
	fmt.Printf("\nCPU and Memory usage by TAP: %+v\n", AppMetric.PodsMetric)
}

func writeCPUAndMemUsageIntoCSV() {

	file, err := os.Create("cpu_mem_usage2.csv")
	if err != nil {
		log.Fatalln("File creation error: \n", err.Error())
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	row := []string{"AppName", "PodName", "ContainerName", "CPU Usage", "Memory Usage", "CPU Requested", "Memory Requested", "CPU Limits", "Memory Limits"}
	if err := w.Write(row); err != nil {
		log.Fatalln("error writing record to file", err)
		return
	}

	AppMetric.Lock.Lock()
	defer AppMetric.Lock.Unlock()
	for appName, podMetrics := range AppMetric.PodsMetric {
		row = []string{appName}
		if err := w.Write(row); err != nil {
			log.Fatalln("error writing record to file", err)
			return
		}
		for _, podMetric := range podMetrics {
			row = []string{" ", podMetric.PodName, " ", convertCPUUnitToMili(podMetric.TotalCPU), convertMemoryUnitToMB(podMetric.TotalMemory)}
			if err := w.Write(row); err != nil {
				log.Fatalln("error writing record to file", err)
				return
			}
			for _, container := range podMetric.Containers {
				row = []string{" ", " ", container.Name, convertCPUUnitToMili(container.Usage.Cpu), convertMemoryUnitToMB(container.Usage.Memory), container.CPUReq, container.MemReq, container.CPULimit, container.MemLimit}
				if err := w.Write(row); err != nil {
					log.Fatalln("error writing record to file", err)
					return
				}
			}
		}

	}
}

func convertMemoryUnitToMB(valueInKB string) string {

	valueInKB = strings.ReplaceAll(valueInKB, " ", "")
	if valueInKB == "" {
		return valueInKB
	}

	endChar := valueInKB[len(valueInKB)-2:]
	if endChar == "Mi" || endChar == "Gi" {
		return valueInKB
	}

	valueFloat, err := strconv.ParseFloat(strings.Split(valueInKB, "Ki")[0], 32)
	if err != nil {
		fmt.Errorf("Error in converting string [%s] into float1: %s\n", valueInKB, err.Error())
		return " "
	}

	valueFloat /= 1024.0

	return fmt.Sprintf("%.3fMi", valueFloat)
}

func convertCPUUnitToMili(valueInNano string) string {

	valueInNano = strings.ReplaceAll(valueInNano, " ", "")
	endChar := valueInNano[len(valueInNano)-1:]
	if valueInNano == "" || endChar == "m" {
		return valueInNano
	}

	valueFloat, err := strconv.ParseFloat(valueInNano[:len(valueInNano)-1], 32)
	if err != nil {
		fmt.Errorf("Error in converting string [%s] to float2: %s\n", valueInNano, err.Error())
		return " "
	}

	if endChar == "n" {
		valueFloat /= 1000000
	} else if endChar == "u" {
		valueFloat /= 1000
	}

	return fmt.Sprintf("%.3fm", valueFloat)
}
