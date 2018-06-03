package stub

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"reflect"

	"github.com/camilocot/cassandra-operator/pkg/apis/database/v1alpha1"

	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/operator-framework/operator-sdk/pkg/util/k8sutil"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubernetes/pkg/util/pointer"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func NewHandler() sdk.Handler {
	return &Handler{}
}

type Handler struct {
	// Fill me
}

func (h *Handler) Handle(ctx context.Context, event sdk.Event) error {
	updated := false
	switch o := event.Object.(type) {
	case *v1alpha1.Cassandra:
		cassandra := o
		// Ignore the delete event since the garbage collector will clean up all secondary resources for the CR
		// All secondary resources must have the CR set as their OwnerReference for this to be the case
		if event.Deleted {
			return nil
		}

		o.SetDefaults()
		// Create the headless service if it doesn't exist
		svc := headLessServiceUnreadyForCassandra(cassandra)

		err := sdk.Get(svc)
		if err != nil {
			err = sdk.Create(svc)
			if err != nil {
				return fmt.Errorf("failed to create headless unready service: %v", err)
			}
		}

		// Create the statefulset if it doesn't exist
		stateful := statefulsetForCassandra(cassandra)
		err = sdk.Create(stateful)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create statefulset: %v", err)
		}

		// Ensure the statefulset size is the same as the spec
		err = sdk.Get(stateful)
		if err != nil {
			return fmt.Errorf("failed to get statefulset: %v", err)
		}

		size := cassandra.Spec.Size

		if *stateful.Spec.Replicas != size {
			if *stateful.Spec.Replicas > size {
				if *stateful.Spec.Replicas != size+1 {
					return fmt.Errorf("statefulset could not be updated, instance decommission can only be done 1 by 1 ")
				}
				fmt.Println("Start decommission of cassandra-cluster-" + fmt.Sprint(size))
				out, _ := ExecCommand(cassandra, "cassandra-cluster-"+fmt.Sprint(size), "nodetool", "decommission")

				fmt.Println(out)
			}
			stateful.Spec.Replicas = &size
			updated = true
		}

		image := cassandra.Spec.Repository + ":" + cassandra.Spec.Version

		if stateful.Spec.Template.Spec.Containers[0].Image != image {
			stateful.Spec.Template.Spec.Containers[0].Image = image
			updated = true
		}

		partition := cassandra.Spec.Partition

		if *stateful.Spec.UpdateStrategy.RollingUpdate.Partition != partition {
			stateful.Spec.UpdateStrategy.RollingUpdate.Partition = &partition
			updated = true
		}

		if updated {
			err = sdk.Update(stateful)
			if err != nil {
				return fmt.Errorf("failed to update statefulset: %v", err)
			}
		}
		podList := podList()
		labelSelector := labels.SelectorFromSet(labelsForCassandra(cassandra.Name)).String()
		listOps := &metav1.ListOptions{LabelSelector: labelSelector}
		err = sdk.List(cassandra.Namespace, podList, sdk.WithListOptions(listOps))
		if err != nil {
			return fmt.Errorf("failed to list pods: %v", err)
		}
		podNames := getPodNames(podList.Items)
		if !reflect.DeepEqual(podNames, cassandra.Status.Nodes) {
			cassandra.Status.Nodes = podNames
			err := sdk.Update(cassandra)
			if err != nil {
				return fmt.Errorf("failed to update cassandra status: %v", err)
			}
		}

	}
	return nil
}

// statefulsetForCassandra returns a cassandra StatefulSet object
func statefulsetForCassandra(c *v1alpha1.Cassandra) *appsv1.StatefulSet {
	labels := labelsForCassandra(c.Name)
	replicas := c.Spec.Size
	partition := c.Spec.Partition
	storageClass := c.Spec.StorageClassName
	env := append(c.Spec.CassandraEnv, v1.EnvVar{
		Name: "POD_IP",
		ValueFrom: &v1.EnvVarSource{
			FieldRef: &v1.ObjectFieldSelector{
				FieldPath: "status.podIP",
			},
		},
	})

	stateful := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.Name,
			Namespace: c.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: c.Name + "-unready",
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			PodManagementPolicy: appsv1.OrderedReadyPodManagement,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
					Partition: &partition,
				},
			},
			RevisionHistoryLimit: pointer.Int32Ptr(10),
			VolumeClaimTemplates: []v1.PersistentVolumeClaim{
				{
					Spec: v1.PersistentVolumeClaimSpec{
						AccessModes: []v1.PersistentVolumeAccessMode{"ReadWriteOnce"},
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
						StorageClassName: &storageClass,
					},
					ObjectMeta: metav1.ObjectMeta{
						Name: "cassandra",
					},
				},
			},

			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "cassandra",
							Image: c.Spec.Repository + ":" + c.Spec.Version,
							Env:   env,
							Ports: []v1.ContainerPort{
								{
									Name:          "cql",
									ContainerPort: 9042,
								},
								{
									Name:          "intra-node",
									ContainerPort: 7001,
								},
								{
									Name:          "jmx",
									ContainerPort: 7099,
								},
							},
							SecurityContext: &v1.SecurityContext{
								Capabilities: &v1.Capabilities{
									Add: []v1.Capability{"IPC_LOCK"},
								},
							},
							ReadinessProbe: &v1.Probe{
								Handler: v1.Handler{
									Exec: &v1.ExecAction{
										Command: []string{"/bin/bash", "-c", "/ready-probe.sh"},
									},
								},
								InitialDelaySeconds: 15,
								TimeoutSeconds:      5,
							},
							Lifecycle: &v1.Lifecycle{
								PreStop: &v1.Handler{
									Exec: &v1.ExecAction{
										Command: []string{"/bin/sh", "-c", "nodetool", "drain"},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	addOwnerRefToObject(stateful, asOwner(c))
	return stateful
}

func headLessServiceUnreadyForCassandra(c *v1alpha1.Cassandra) *v1.Service {

	labels := labelsForCassandra(c.Name)

	svc := &v1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.Name + "-unready",
			Labels:    labels,
			Namespace: c.Namespace,
			// it will return IPs even of the unready pods. Bootstraping a new cluster need it
			Annotations: map[string]string{
				"service.alpha.kubernetes.io/tolerate-unready-endpoints": "true",
			},
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       "cql",
					Port:       9042,
					TargetPort: intstr.FromInt(9042),
					Protocol:   v1.ProtocolTCP,
				},
			},
			Selector:  labels,
			ClusterIP: "None",
			Type:      v1.ServiceTypeClusterIP,
		},
	}
	addOwnerRefToObject(svc, asOwner(c))
	return svc
}

// labelsForCassadnra returns the labels for selecting the resources
// belonging to the given casandra CR name.
func labelsForCassandra(name string) map[string]string {
	return map[string]string{
		"app":          "cassandra",
		"cassandra_cr": name,
	}
}

// addOwnerRefToObject appends the desired OwnerReference to the object
func addOwnerRefToObject(obj metav1.Object, ownerRef metav1.OwnerReference) {
	obj.SetOwnerReferences(append(obj.GetOwnerReferences(), ownerRef))
}

// asOwner returns an OwnerReference set as the cassandra CR
func asOwner(c *v1alpha1.Cassandra) metav1.OwnerReference {
	trueVar := true
	return metav1.OwnerReference{
		APIVersion: c.APIVersion,
		Kind:       c.Kind,
		Name:       c.Name,
		UID:        c.UID,
		Controller: &trueVar,
	}
}

// podList returns a v1.PodList object
func podList() *v1.PodList {
	return &v1.PodList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
	}
}

// getPodNames returns the pod names of the array of pods passed in
func getPodNames(pods []v1.Pod) []string {
	var podNames []string
	for _, pod := range pods {
		podNames = append(podNames, pod.Name)
	}
	return podNames
}

func ExecCommand(c *v1alpha1.Cassandra, podName string, command ...string) (string, error) {

	var (
		execOut bytes.Buffer
		execErr bytes.Buffer
	)

	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: c.Namespace,
		},
	}

	err := sdk.Get(pod)
	if err != nil {
		return "", fmt.Errorf("could not get pod info: %v", err)
	}

	if len(pod.Spec.Containers) != 1 {
		return "", fmt.Errorf("could not determine which container to use")
	}

	kubeClient, kubeConfig := mustNewKubeClientAndConfig()

	req := kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec")
	req.VersionedParams(&v1.PodExecOptions{
		Container: pod.Spec.Containers[0].Name,
		Command:   command,
		Stdout:    true,
		Stderr:    true,
	}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(kubeConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to init executor: %v", err)
	}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: &execOut,
		Stderr: &execErr,
		Tty:    false,
	})

	if err != nil {
		return "", fmt.Errorf("could not execute: %v", err)
	}

	if execErr.Len() > 0 {
		return "", fmt.Errorf("stderr: %v", execErr.String())
	}

	return execOut.String(), nil
}

// mustNewKubeClientAndConfig returns the in-cluster config and kubernetes client
// or if KUBERNETES_CONFIG is given an out of cluster config and client
func mustNewKubeClientAndConfig() (kubernetes.Interface, *rest.Config) {
	var cfg *rest.Config
	var err error
	if os.Getenv(k8sutil.KubeConfigEnvVar) != "" {
		cfg, err = outOfClusterConfig()
	} else {
		cfg, err = inClusterConfig()
	}
	if err != nil {
		panic(err)
	}
	return kubernetes.NewForConfigOrDie(cfg), cfg
}

// inClusterConfig returns the in-cluster config accessible inside a pod
func inClusterConfig() (*rest.Config, error) {
	// Work around https://github.com/kubernetes/kubernetes/issues/40973
	// See https://github.com/coreos/etcd-operator/issues/731#issuecomment-283804819
	if len(os.Getenv("KUBERNETES_SERVICE_HOST")) == 0 {
		addrs, err := net.LookupHost("kubernetes.default.svc")
		if err != nil {
			return nil, err
		}
		os.Setenv("KUBERNETES_SERVICE_HOST", addrs[0])
	}
	if len(os.Getenv("KUBERNETES_SERVICE_PORT")) == 0 {
		os.Setenv("KUBERNETES_SERVICE_PORT", "443")
	}
	return rest.InClusterConfig()
}

func outOfClusterConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv(k8sutil.KubeConfigEnvVar)
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	return config, err
}
