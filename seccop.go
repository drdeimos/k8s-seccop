package main

import (
    "fmt"
    "log"
    "os"
    "flag"

    corev1 "k8s.io/api/core/v1"
    types  "k8s.io/client-go/kubernetes/typed/core/v1"
    "k8s.io/apimachinery/pkg/util/runtime"
    "k8s.io/klog"

    "k8s.io/client-go/informers"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/tools/cache"
    "k8s.io/client-go/tools/clientcmd"
)

const (
  COPIER_LABLE = "secret-copier"
)

func main() {
  klog.InitFlags(nil)
  flag.Parse()

  klog.Info("Secret-copier app started")
  // TODO Read config also in-cluster
  // Read config out-cluster
  kubeconfig := os.Getenv("KUBECONFIG")
  config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
  if err != nil {
      log.Panic(err.Error())
  }

  // Client for informer
  clientset, err := kubernetes.NewForConfig(config)
  if err != nil {
      log.Panic(err.Error())
  }

  // Client for create secrets
  clientSecret := clientset.CoreV1().Secrets("production")

  // TODO: make informer watch only for default ns
  factory := informers.NewSharedInformerFactory(clientset, 0)
  informer := factory.Core().V1().Secrets().Informer()
  stopper := make(chan struct{})
  defer close(stopper)
  defer runtime.HandleCrash()
  informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
      AddFunc: func(obj interface{}) {
                onAdd(obj, clientSecret)
      },

  })
  go informer.Run(stopper)
  if !cache.WaitForCacheSync(stopper, informer.HasSynced) {
      runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
      return
  }
  <-stopper
}

func onAdd(obj interface{}, clientSecret types.SecretInterface) {
  secret := obj.(*corev1.Secret)

  _, ok := secret.GetLabels()[COPIER_LABLE]
  if ok {
    secretNamespace := secret.ObjectMeta.Namespace
    secretName := secret.ObjectMeta.Name
    secretLabels := secret.GetLabels()
    klog.Info("Found: namespace: ", secretNamespace, ", name: ", secretName, ", labels: ", secretLabels)
    klog.V(2).Info("Get annotations of ", secretNamespace, "/", secretName)
    annotations := secret.ObjectMeta.GetAnnotations()
    if origin, ok := annotations["secret-copier/origin"]; ok {
      klog.V(2).Info("Annotation exist. Val: ", origin,"; ok:", ok)
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
        klog.Info("Created copy: namespace: ", newSecretNamespace, ", name: ", newSecretName, ", labels: ", newSecretLabels)
        klog.V(2).Info("Try create object: ", newSecretNamespace, "/", newSecretName)
        _, err := clientSecret.Create(newSecret)
        if err != nil {
          klog.Info("Err: ", err)
        } else {
          klog.Info("Created: ", newSecretNamespace, "/", newSecretName)
        }
      }
    }
  }
}
