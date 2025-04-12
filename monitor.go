package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

var listenOn = ":8080"

var mqttClient mqtt.Client

var feedRefreshTopicPrefix = "k8s-informers/refresh"

var podLister informers.SharedInformerFactory

func getKubeConfig() (*rest.Config, error) {
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}

type PodBrief struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	App       string `json:"app"`
	PodIP     string `json:"podIP"`
}

func getBriefPodList(namespaceFilter *string, appFilter *string) ([]PodBrief, error) {
	pods, err := podLister.Core().V1().Pods().Lister().List(labels.Everything())
	if err != nil {
		// http.Error(w, fmt.Sprintf("Failed to list pods: %v", err), http.StatusInternalServerError)
		return nil, err
	}

	// output only metadata.name ,  metadata.namespace , metadata.labels.app ,  status.podIP

	var briefList []PodBrief
	for _, pod := range pods {
		if namespaceFilter != nil && pod.Namespace != *namespaceFilter {
			continue
		}
		if appFilter != nil && pod.Labels["app"] != *appFilter {
			continue
		}
		briefList = append(briefList, PodBrief{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			App:       pod.Labels["app"],
			PodIP:     pod.Status.PodIP,
		})
	}

	return briefList, nil
}

func listPodsHandler(w http.ResponseWriter, r *http.Request) {
	briefList, err := getBriefPodList(nil, nil) // no filters
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list pods: %v", err), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(briefList)
}

func listNamespaceHandler(w http.ResponseWriter, r *http.Request) {

	// extract namespace from request path /ns/{namespace}
	namespace := r.URL.Path[len("/ns/"):]
	briefList, err := getBriefPodList(&namespace, nil) // filter by namespace
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list pods: %v", err), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(briefList)
}

func listAppHandler(w http.ResponseWriter, r *http.Request) {
	// extract app from request path /app/{app}
	app := r.URL.Path[len("/app/"):]
	briefList, err := getBriefPodList(nil, &app) // filter by app
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list pods: %v", err), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(briefList)
}

func listFeedHandler(w http.ResponseWriter, r *http.Request) {
	// extract app from request path /app/{app}
	feedPart := r.URL.Path[len("/feed/"):]

	// feedPart is clusterId_namespace_app
	parts := strings.Split(feedPart, "_")
	if len(parts) != 3 {
		http.Error(w, fmt.Sprintf("Invalid feed name: %s", feedPart), http.StatusBadRequest)
		return
	}
	// clusterId := parts[0]
	namespace := parts[1]
	app := parts[2]

	briefList, err := getBriefPodList(&namespace, &app) // filter by app
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list pods: %v", err), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(briefList)
}

// Define a custom flag type that implements the flag.Value interface
type StringList []string

// Implement the String() method to satisfy the flag.Value interface
func (s *StringList) String() string {
	return fmt.Sprintf("%v", *s)
}

// Implement the Set() method to parse the flag value
func (s *StringList) Set(value string) error {

	// fmt.Printf("Set value: %v\n", value)
	// split value to array of strings
	parts := strings.Split(value, ",")
	for _, part := range parts {
		//appent trimmed part to the list
		*s = append(*s, strings.TrimSpace(part))
	}
	return nil
}

func (s *StringList) Contains(value string) bool {
	for _, item := range *s {
		// fmt.Println("Contains comparing item:", item, "value:", value)
		if item == value {
			return true
		}
	}
	return false
}

var ns StringList
var app StringList
var mqttBroker = "tcp://localhost:1882"
var mqttTopicPrefix = "k8s-informers"
var mqttClientID = "k8s-informers"

// enum ADD, UPDATE, DELETE
type PodIPEventType string // iota is reset to 0
const (
	ADD    PodIPEventType = "ADD"
	UPDATE                = "UPDATE"
	DELETE                = "DELETE"
)

type PodIPEvent struct {
	Type      PodIPEventType `json:"type"`
	ClusterId string         `json:"clusterId"`
	Pod       PodBrief       `json:"pod"`
}

type IPChange struct {
	Add    string `json:"add"`
	Delete string `json:"delete"`
}

func processPodIPEvent(event PodIPEvent) {
	namespace := event.Pod.Namespace
	deployment := event.Pod.App

	// fmt.Println("does ns contain namespace:", namespace, ns, ns.Contains(namespace))
	if len(ns) > 0 && !ns.Contains(namespace) {
		return
	}
	// fmt.Println("does app contain deployment:", deployment, app, app.Contains(deployment))
	if len(app) > 0 && !app.Contains(deployment) {
		return
	}
	fmt.Println("Processing pod IP event:", event)

	topic := fmt.Sprintf("%s/%s/%s/%s", mqttTopicPrefix, event.Pod.Namespace, event.Pod.App, event.Pod.Name)
	payload, err := json.Marshal(event)
	if err != nil {
		fmt.Printf("Failed to marshal event: %v\n", err)
		return
	}

	if token := mqttClient.Publish(topic, 0, false, payload); token.Wait() && token.Error() != nil {
		fmt.Printf("Failed to publish message: %v\n", token.Error())
	}

	payloadData := IPChange{}
	if event.Type == ADD {
		payloadData.Add = event.Pod.PodIP
	} else if event.Type == DELETE {
		payloadData.Delete = event.Pod.PodIP
	}
	changePayload, err := json.Marshal(payloadData)
	if err != nil {
		fmt.Printf("Failed to marshal event: %v\n", err)
		return
	}

	// publish NS feed refresh event
	nsFeedTopic := fmt.Sprintf("%s/ns/%s", feedRefreshTopicPrefix, event.Pod.Namespace)
	nsFeedPayload := "{}"
	if changePayload != nil {
		nsFeedPayload = string(changePayload)
	}
	if token := mqttClient.Publish(nsFeedTopic, 0, false, []byte(nsFeedPayload)); token.Wait() && token.Error() != nil {
		fmt.Printf("Failed to publish message: %v\n", token.Error())
	}

	// publish App feed refresh event
	appFeedTopic := fmt.Sprintf("%s/app/%s", feedRefreshTopicPrefix, event.Pod.App)
	appFeedPayload := "{}"
	if changePayload != nil {
		appFeedPayload = string(changePayload)
	}
	if token := mqttClient.Publish(appFeedTopic, 0, false, []byte(appFeedPayload)); token.Wait() && token.Error() != nil {
		fmt.Printf("Failed to publish message: %v\n", token.Error())
	}

	feedName := fmt.Sprintf("%s_%s_%s", event.ClusterId, event.Pod.Namespace, event.Pod.App)
	feedTopic := fmt.Sprintf("%s/feed/%s", feedRefreshTopicPrefix, feedName)
	feedPayload := "{}"
	if changePayload != nil {
		feedPayload = string(changePayload)
	}
	if token := mqttClient.Publish(feedTopic, 0, false, []byte(feedPayload)); token.Wait() && token.Error() != nil {
		fmt.Printf("Failed to publish message: %v\n", token.Error())
	}

}

func connectMqttClient(tlsConfig *tls.Config) mqtt.Client {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(mqttBroker)
	opts.SetClientID(mqttClientID)
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetTLSConfig(tlsConfig)

	opts.OnConnect = func(c mqtt.Client) {
		fmt.Println("Connected to MQTT broker")
	}
	opts.OnConnectionLost = func(c mqtt.Client, err error) {
		fmt.Printf("Connection lost: %v\n", err)
	}

	fmt.Printf("Connecting to broker: %s\n", mqttBroker)
	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		fmt.Printf("Failed to connect to broker: %v\n", token.Error())
		os.Exit(1)
	}

	// subscribe to test/topic
	// if token := client.Subscribe(topic, 0, func(client mqtt.Client, msg mqtt.Message) {
	// 	fmt.Printf("Received message: %s\n", msg.Payload())
	// }); token.Wait() && token.Error() != nil {
	// 	fmt.Printf("Failed to subscribe to topic: %v\n", token.Error())
	// 	//os.Exit(1)
	// }

	return client
}

var clusterId string

// store last known IP address for a pod in thread safe map
// key is pod key: namespace/name
// value is IP address
var lastKnownIpMap sync.Map

func createTLSConfig(caFile string, certFile string, keyFile string) (*tls.Config, error) {

	fmt.Println("CA file: ", caFile)
	fmt.Println("Cert file: ", certFile)
	fmt.Println("Key file: ", keyFile)

	// Configure TLS settings
	tlsConfig := &tls.Config{
		// RootCAs:      caCertPool,
		// Certificates: []tls.Certificate{clientCert},
	}

	// Load CA certificate
	if caFile != "" {
		caCert, err := ioutil.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %v", err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = caCertPool
	}

	// Load client certificate and key
	if certFile != "" && keyFile != "" {
		clientCert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate and key: %v", err)
		}
		// fmt.Printf("Client certificate: %v\n", clientCert)
		tlsConfig.Certificates = []tls.Certificate{clientCert}
	}

	return tlsConfig, nil
}

func main() {

	//labelSelector := flag.String("label", "app=my-app", "Label selector for filtering pods")
	fmt.Println("ns:", ns)
	fmt.Println("app:", app)
	fmt.Println("ops:", PodIPEventType(ADD), PodIPEventType(UPDATE), PodIPEventType(DELETE))

	clusterIdFlag := flag.String("clusterId", "", "Cluster ID")

	listenOnFlag := flag.String("listenOn", ":8877", "listen on, e.g. :8880")

	// ns - list of strings - monitored namespaces
	// var ns StringList
	flag.Var(&ns, "ns", "Comma-separated list of monitored namespaces")

	// app - list of strings - monitored apps
	// var app StringList
	flag.Var(&app, "app", "Comma-separated list of monitored deployments identified by app label")

	mqttBrokerFlag := flag.String("mqttBroker", "", "MQTT broker URL - e.g. tcp://localhost:1883 or ssl://localhost:8883")
	mqttTopicPrefixFlag := flag.String("mqttTopicPrefix", "", "MQTT topic prefix - e.g. k8s-informers")

	caFlag := flag.String("ca", "", "CA certificate file - e.g. ca.crt/ca.pem")
	certFlag := flag.String("cert", "", "Client certificate file - e.g. client.crt/client.pem")
	keyFlag := flag.String("key", "", "Client key file - e.g. client.key")

	flag.Parse()

	if *listenOnFlag != "" {
		listenOn = *listenOnFlag
	}

	if *mqttBrokerFlag != "" {
		mqttBroker = *mqttBrokerFlag
	}
	if *mqttTopicPrefixFlag != "" {
		mqttTopicPrefix = *mqttTopicPrefixFlag
	}

	// Setup TLS configuration
	tlsConfig, err := createTLSConfig(*caFlag, *certFlag, *keyFlag)
	if err != nil {
		log.Fatalf("Error creating TLS configuration: %v", err)
	}
	// Parse the command-line flags

	mqttClientID = fmt.Sprintf("k8s-informers-%s", time.Now().Format("20060102150405"))
	fmt.Println("Starting MQTT publisher")
	mqttClient = connectMqttClient(tlsConfig)
	fmt.Println("Connected to MQTT broker")

	// Print the result
	fmt.Println("ns:", ns)
	fmt.Println("app:", app)

	// Create Kubernetes client
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = getKubeConfig()
		if err != nil {
			log.Fatalf("Failed to get Kubernetes config: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	if *clusterIdFlag == "" {
		// get cluster ID
		namespace, err := clientset.CoreV1().Namespaces().Get(context.TODO(), "kube-system", metav1.GetOptions{})
		if err != nil {
			log.Fatalf("Failed to fetch kube-system namespace to create clusterId: %v", err)
		} else {
			clusterId = string(namespace.UID)
		}
	} else {
		clusterId = *clusterIdFlag
	}

	// Create informer factory
	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		30*time.Second, // Resync period
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			//opts.LabelSelector = *labelSelector
		}),
	)

	podLister = factory

	podInformer := factory.Core().V1().Pods().Informer()

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			labels := pod.GetLabels()
			log.Printf("Pod added: %s/%s (app=%s), IP: %s", pod.Namespace, pod.Name, labels["app"], pod.Status.PodIP)
			if pod.Status.PodIP != "" {
				newPodIPEvent := PodIPEvent{
					Type:      ADD,
					ClusterId: clusterId,
					Pod: PodBrief{
						Name:      pod.Name,
						Namespace: pod.Namespace,
						App:       labels["app"],
						PodIP:     pod.Status.PodIP,
					},
				}
				processPodIPEvent(newPodIPEvent)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			newPod := newObj.(*corev1.Pod)
			if oldPod.Status.PodIP != newPod.Status.PodIP {
				// if Pod is losing IP - from non empry oldPod.Status.PodIP to empty newPod.Status.PodIP
				// then it is being deleted and we want to record the last known IP
				log.Printf("old IP: %v, new IP: %v", oldPod.Status.PodIP, newPod.Status.PodIP)
				if oldPod.Status.PodIP != "" && newPod.Status.PodIP == "" {
					podKey := fmt.Sprintf("%s/%s", oldPod.Namespace, oldPod.Name)
					lastKnownIpMap.Store(podKey, oldPod.Status.PodIP)
					// log.Println("here recording last known IP")
				}
				// we can also release last known IP address in case we go from empty to non empty

				labels := newPod.GetLabels()
				log.Printf("Pod IP changed: %s/%s (app=%s), Old IP: %s, New IP: %s", newPod.Namespace, newPod.Name, labels["app"], oldPod.Status.PodIP, newPod.Status.PodIP)
				if oldPod.Status.PodIP == "" {
					newPodIPEvent := PodIPEvent{
						// Type:      UPDATE, // we can use ADD event for this case as IP is added
						Type:      ADD,
						ClusterId: clusterId,
						Pod: PodBrief{
							Name:      newPod.Name,
							Namespace: newPod.Namespace,
							App:       labels["app"],
							PodIP:     newPod.Status.PodIP,
						},
					}
					processPodIPEvent(newPodIPEvent)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			labels := pod.GetLabels()
			log.Printf("Pod deleted: %s/%s (app=%s), Last known IP: %s", pod.Namespace, pod.Name, labels["app"], pod.Status.PodIP)

			podIP := pod.Status.PodIP
			if podIP == "" {
				// if pod is deleted and it has no IP address, we use last known IP address
				podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
				if lastKnownIp, ok := lastKnownIpMap.Load(podKey); ok {
					podIP = lastKnownIp.(string)
					// entry no more needed, we can delete it
					lastKnownIpMap.Delete(podKey)
				}
			}

			newPodIPEvent := PodIPEvent{
				Type:      DELETE,
				ClusterId: clusterId,
				Pod: PodBrief{
					Name:      pod.Name,
					Namespace: pod.Namespace,
					App:       labels["app"],
					PodIP:     podIP,
				},
			}
			processPodIPEvent(newPodIPEvent)
		},
	})

	stopCh := make(chan struct{})
	defer close(stopCh)

	log.Println("Starting pod informer...")
	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)

	// Start web server
	http.HandleFunc("/pods", listPodsHandler)
	http.HandleFunc("/ns/", listNamespaceHandler)
	http.HandleFunc("/app/", listAppHandler)
	http.HandleFunc("/feed/", listFeedHandler)

	go func() {
		log.Printf("Starting web server on %s...", listenOn)
		if err := http.ListenAndServe(listenOn, nil); err != nil {
			log.Fatalf("Failed to start web server: %v", err)
		}
	}()

	<-stopCh // Block forever
}
