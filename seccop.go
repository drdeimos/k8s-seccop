package main

import (
    "fmt"
    "os"
    "flag"
    "path/filepath"

    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/util/runtime"
    "k8s.io/klog"

    "k8s.io/client-go/informers"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/util/homedir"
    "k8s.io/client-go/tools/cache"
    "k8s.io/client-go/tools/clientcmd"

    "encoding/hex"
    "io"
    "crypto/rand"
    "github.com/minio/highwayhash"
    "bytes"
)

const (
  COPIER_LABEL = "secret-copier"
)

var (
  config     rest.Config
  hashKey    []byte
  kubeconfig string
  masterURL  string
)

func main() {
  klog.InitFlags(nil)
  flag.Parse()
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

  _, ok := secret.GetLabels()[COPIER_LABEL]
  if ok {

    // Client for create secrets
    clientSecret := clientset.CoreV1().Secrets("production")

    secretNamespace := secret.ObjectMeta.Namespace
    secretName := secret.ObjectMeta.Name
    secretLabels := secret.GetLabels()
    klog.V(2).Info("Found secret. Namespace: ", secretNamespace, ", name: ", secretName, ", labels: ", secretLabels)
    klog.V(2).Info("Get annotations of ", secretNamespace, "/", secretName)
    annotations := secret.ObjectMeta.GetAnnotations()
    if origin, ok := annotations["secret-copier/origin"]; ok {
      klog.V(2).Info("Annotation exist. Val: ", origin,". Skip")
    } else {
      klog.V(2).Info("Check origin passed. Step 1. ", origin)
      if origin != "clone" {
        klog.V(2).Info("Check origin passed. Step 2. ", origin)
        klog.Info("Read: namespace: ", secretNamespace, ", name: ", secretName, ", labels: ", secretLabels)

        newSecret := secret.DeepCopy()
        newSecret.ObjectMeta.Namespace = "production"

        // Clean fields
        newSecret.ObjectMeta.SetResourceVersion("")
        newSecret.ObjectMeta.SetSelfLink("")
        newSecret.ObjectMeta.SetUID("")
        newAnnotations := secret.ObjectMeta.GetAnnotations()
        delete(newAnnotations,"kubectl.kubernetes.io/last-applied-configuration")
        //newSecret.ObjectMeta.SetCreationTimestamp(nil)
        // End

        // Add annotation about copy
        newAnnotations["secret-copier/origin"] = "clone"
        newSecret.ObjectMeta.SetAnnotations(newAnnotations)

        newSecretNamespace := newSecret.ObjectMeta.Namespace
        newSecretName := newSecret.ObjectMeta.Name
        newSecretLabels := newSecret.GetLabels()
        klog.V(2).Info("Prepared copy: namespace: ", newSecretNamespace, ", name: ", newSecretName, ", labels: ", newSecretLabels)

        // Check secret exists
        existSecret, err := clientSecret.Get(newSecretName, metav1.GetOptions{})
        if err != nil {
          klog.V(2).Info("Secret don't exist. Check passed")
          // Create cloned secret
          klog.V(2).Info("Try create object: ", newSecretNamespace, "/", newSecretName)
          _, err = clientSecret.Create(newSecret)
          if err != nil {
            klog.Info("Err: ", err)
          } else {
            klog.Info("Created: ", newSecretNamespace, "/", newSecretName)
          }
        } else {
          klog.V(2).Info("Secret exist: ", newSecretNamespace, "/", newSecretName, ".")
          // Compare data && update
          klog.V(2).Info("Prepare..")
          existSecretBytes := keysToByte(existSecret.Data)
          newSecretBytes := keysToByte(newSecret.Data)
          klog.V(2).Info("Compare..")

          // Compute hash for exist
          existHash, err := highwayhash.New(hashKey) // Seed hash instance
          if err != nil {
            klog.Fatal("Failed to create HighwayHash instance: ", err)
          }

          if _, err = io.Copy(existHash, bytes.NewReader(existSecretBytes)); err != nil {
            klog.Fatal("Failed to read existSecretBytes: ", err) // add error handling
          }

          existChecksum := existHash.Sum(nil)
          existChecksumString := hex.EncodeToString(existChecksum)
          klog.V(2).Info("Checksum for exist secret data:", existChecksumString)

          // Compute hash for new
          newHash, err := highwayhash.New(hashKey) // Seed hash instance
          if err != nil {
            klog.Fatal("Failed to create HighwayHash instance: ", err)
          }

          if _, err = io.Copy(newHash, bytes.NewReader(newSecretBytes)); err != nil {
            klog.Fatal("Failed to create HighwayHash instance: ", err)
          }

          newChecksum := newHash.Sum(nil)
          newChecksumString := hex.EncodeToString(newChecksum)
          klog.V(2).Info("Checksum for new secret data:", newChecksumString)

          if existChecksumString != newChecksumString {
            _, err = clientSecret.Update(newSecret)
            if err != nil {
              klog.Info("Err: ", err)
            } else {
              klog.Info("Updated: ", newSecretNamespace, "/", newSecretName)
            }
          } else {
            klog.V(2).Info("Secret data already actual: ", newSecretNamespace, "/", newSecretName)
          }
          return
        }
      }
    }
  }
}

func onAddNamespace(obj interface{}, clientset kubernetes.Clientset) {
  namespace := obj.(*corev1.Namespace)

  klog.V(2).Info("Found namespace. Name: ", namespace.ObjectMeta.Name)
}

func appInit() {
  klog.V(2).Info("Init")
  hashKey, _ = randomHex(32)
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
