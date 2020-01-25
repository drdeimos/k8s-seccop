package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/minio/highwayhash"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog"
)

const (
	// CopierLabel for secrets in need of copying
	CopierLabel = "secret-copier"
)

var (
	config     rest.Config
	hashKey    []byte
	kubeconfig string
	masterURL  string
	nslist     = struct {
		sync.RWMutex
		m map[string]int
	}{m: make(map[string]int)}
	secretlist = struct {
		sync.RWMutex
		m map[string]map[string]int
	}{m: make(map[string]map[string]int)}
)

func main() {
	appInit()

	klog.Info("Secret-copier app started")

	// Read the in-cluster config
	klog.V(2).Info("Try load in-cluster config")
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.V(2).Info("In-cluster config load failed")
		// Read config out-cluster
		if len(os.Getenv("KUBECONFIG")) < 5 {
			if home := homedir.HomeDir(); home != "" {
				klog.V(2).Info("Try load out-cluster config load from discovered path")
				kubeconfig = *flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
				klog.V(2).Info("Out-cluster config loaded from discovered path: ", kubeconfig)
			} else {
				klog.V(2).Info("Try load out-cluster config load from flag with path")
				kubeconfig = *flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
				klog.V(2).Info("Out-cluster config loaded from flag with path:", kubeconfig)
			}
		} else {
			klog.V(2).Info("Out-cluster config loaded from env KUBECONFIG:", os.Getenv("KUBECONFIG"))
			kubeconfig = os.Getenv("KUBECONFIG")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			klog.Fatal("Cluster config load failed")
		}
	}

	// Client for informer
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal("Create client failed", err.Error())
	}

	factory := informers.NewSharedInformerFactory(clientset, 0)

	// Informer for secrets
	klog.V(2).Info("Create secrets informer")
	informerSecrets := factory.Core().V1().Secrets().Informer()
	stopper := make(chan struct{})
	defer close(stopper)
	defer runtime.HandleCrash()

	klog.V(2).Info("Add handler for secrets informer")
	informerSecrets.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			onAddSecret(obj, *clientset)
		},
		DeleteFunc: func(obj interface{}) {
			onDelSecret(obj, *clientset)
		},
	})

	klog.V(2).Info("Run secrets informer")
	go informerSecrets.Run(stopper)
	klog.V(2).Info("Runned")

	// Informer for namespaces
	informerNamespaces := factory.Core().V1().Namespaces().Informer()
	informerNamespaces.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			onAddNamespace(obj, *clientset)
		},
		DeleteFunc: func(obj interface{}) {
			onDelNamespace(obj, *clientset)
		},
	})
	klog.V(2).Info("Run namespaces informer")
	go informerNamespaces.Run(stopper)
	klog.V(2).Info("Runned")

	// WaitForCacheSync TODO may be run this sequental is wrong
	if !cache.WaitForCacheSync(stopper, informerSecrets.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}
	<-stopper

	if !cache.WaitForCacheSync(stopper, informerNamespaces.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}
	<-stopper
}

func onAddSecret(obj interface{}, clientset kubernetes.Clientset) {
	secret := obj.(*corev1.Secret)

	_, ok := secret.GetLabels()[CopierLabel]
	if ok {

		secretListAdd(secretlist.m, secret.GetNamespace(), secret.GetName())

		klog.V(2).Info("Map ns contain: ", nslist.m)
		klog.V(2).Info("Map secret contain: ", secretlist.m)

		//FIXME
		for namespace := range nslist.m {
			klog.V(2).Info("Run secretCopy() for: ", namespace)
			secretCopy(obj, clientset, namespace)
		}
	}
}

func onAddNamespace(obj interface{}, clientset kubernetes.Clientset) {
	namespace := obj.(*corev1.Namespace)

	klog.V(3).Info("Found namespace. Name: ", namespace.ObjectMeta.Name)
	nslist.m[namespace.ObjectMeta.Name] = 1
	klog.V(3).Info("Map ns contain: ", nslist.m)
}

func onDelSecret(obj interface{}, clientset kubernetes.Clientset) {
	secret := obj.(*corev1.Secret)

	klog.V(2).Info("Removed secret. Name: ", secret.ObjectMeta.Name)
	secretListDel(secretlist.m, secret.GetNamespace(), secret.GetName())
	klog.V(2).Info("Map ns contain: ", nslist.m)
}

func onDelNamespace(obj interface{}, clientset kubernetes.Clientset) {
	namespace := obj.(*corev1.Namespace)

	klog.V(2).Info("Removed namespace. Name: ", namespace.ObjectMeta.Name)
	nslist.RLock()
	delete(nslist.m, namespace.ObjectMeta.Name)
	nslist.RUnlock()
	klog.V(2).Info("Map ns contain: ", nslist.m)
}

func appInit() {
	klog.V(2).Info("Init")
	hashKey, _ = randomHex(32)
	nslist.m = make(map[string]int)
	secretlist.m = make(map[string]map[string]int)

	klog.InitFlags(nil)
	flag.Parse()
}

func randomHex(n int) ([]byte, error) {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return nil, err
	}
	return bytes, nil
}

func keysToByte(data map[string][]uint8) []byte {
	b := new(bytes.Buffer)
	for key, value := range data {
		fmt.Fprintf(b, "%s=\"%s\";", key, value)
	}
	return b.Bytes()
}

func secretsDataEqual(one corev1.Secret, two corev1.Secret) bool {
	// Compare data && update
	klog.V(2).Info("Prepare..")
	oneBytes := keysToByte(one.Data)
	twoBytes := keysToByte(two.Data)
	klog.V(2).Info("Compare..")

	// Compute hash for one
	oneHash, err := highwayhash.New(hashKey) // Seed hash instance
	if err != nil {
		klog.Fatal("Failed to create HighwayHash instance: ", err)
	}

	if _, err = io.Copy(oneHash, bytes.NewReader(oneBytes)); err != nil {
		klog.Fatal("Failed to read oneBytes: ", err) // add error handling
	}

	oneChecksum := oneHash.Sum(nil)
	oneChecksumString := hex.EncodeToString(oneChecksum)
	klog.V(2).Info("Checksum for one secret data:", oneChecksumString)

	// Compute hash for two
	twoHash, err := highwayhash.New(hashKey) // Seed hash instance
	if err != nil {
		klog.Fatal("Failed to create HighwayHash instance: ", err)
	}

	if _, err = io.Copy(twoHash, bytes.NewReader(twoBytes)); err != nil {
		klog.Fatal("Failed to create HighwayHash instance: ", err)
	}

	twoChecksum := twoHash.Sum(nil)
	twoChecksumString := hex.EncodeToString(twoChecksum)
	klog.V(2).Info("Checksum for two secret data:", twoChecksumString)

	res := oneChecksumString == twoChecksumString

	return res
}

func secretListAdd(m map[string]map[string]int, ns, name string) {
	klog.V(2).Info("Add element: ", ns, "[", name, "] in secretlist")
	mm, ok := m[ns]
	if !ok {
		mm = make(map[string]int)
		m[ns] = mm
	}
	mm[name] = 1
}

func secretListDel(m map[string]map[string]int, ns, name string) {
	klog.V(2).Info("Del element: ", ns, "[", name, "] in secretlist")
	delete(m[ns], name)
	if len(m[ns]) == 0 {
		delete(m, ns)
	}
}

func secretCopy(obj interface{}, clientset kubernetes.Clientset, targetNamespace string) {
	//FIXME
	// Client for create secrets
	clientSecret := clientset.CoreV1().Secrets(targetNamespace)
	secret := obj.(*corev1.Secret)
	secretNamespace := secret.ObjectMeta.Namespace
	secretName := secret.ObjectMeta.Name
	secretLabels := secret.GetLabels()
	klog.V(2).Info("Work with secret: ", secretNamespace, "/", secretName, ", labels: ", secretLabels)
	klog.V(2).Info("Get annotations of ", secretNamespace, "/", secretName, ":", secret.ObjectMeta.GetAnnotations())
	annotations := secret.ObjectMeta.GetAnnotations()
	if origin, ok := annotations["secret-copier/origin"]; ok { //FIXME partial condition?
		klog.V(2).Info("Annotation exist. Val: ", origin, ". Skip")
	} else {
		klog.V(2).Info("Check origin passed. Step 1. ", origin)
		if origin != "clone" {
			klog.V(2).Info("Check origin passed. Step 2. ", origin)
			klog.Info("Read: namespace: ", secretNamespace, ", name: ", secretName, ", labels: ", secretLabels)

			newSecret := secret.DeepCopy()
			newSecret.ObjectMeta.Namespace = targetNamespace

			// Clean fields
			newSecret.ObjectMeta.SetResourceVersion("")
			newSecret.ObjectMeta.SetSelfLink("")
			newSecret.ObjectMeta.SetUID("")
			newAnnotations := secret.ObjectMeta.GetAnnotations()
			delete(newAnnotations, "kubectl.kubernetes.io/last-applied-configuration")
			//newSecret.ObjectMeta.SetCreationTimestamp(nil)
			// End

			// Add annotation about copy
			newAnnotations["secret-copier/origin"] = "clone"
			newSecret.ObjectMeta.SetAnnotations(newAnnotations)

			newSecretName := newSecret.ObjectMeta.Name
			newSecretLabels := newSecret.GetLabels()
			klog.V(2).Info("Prepared copy: namespace: ", targetNamespace, ", name: ", newSecretName, ", labels: ", newSecretLabels)

			// Check secret exists
			existSecret, err := clientSecret.Get(newSecretName, metav1.GetOptions{})
			if err != nil {
				klog.V(2).Info("Secret don't exist. Check passed")
				// Create cloned secret
				klog.V(2).Info("Try create object: ", targetNamespace, "/", newSecretName)
				_, err = clientSecret.Create(newSecret)
				if err != nil {
					klog.Info("Err: ", err)
				} else {
					klog.Info("Created: ", targetNamespace, "/", newSecretName)
				}
			} else {
				klog.V(2).Info("Secret exist: ", targetNamespace, "/", newSecretName, ".")
				// Compare
				if secretsDataEqual(*newSecret, *existSecret) {
					// Nothing to do if **data** of secrets actual
					klog.V(2).Info("Secret data already actual: ", targetNamespace, "/", newSecretName)
				} else {
					// Update secret
					_, err = clientSecret.Update(newSecret)
					if err != nil {
						klog.Info("Err: ", err)
					} else {
						klog.Info("Updated: ", targetNamespace, "/", newSecretName)
					}
				}
				return
			}
		}
	}
}
