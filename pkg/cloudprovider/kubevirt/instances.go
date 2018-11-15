package kubevirt

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/cloudprovider"
	kubevirtv1 "kubevirt.io/kubevirt/pkg/api/v1"
)

const (
	instanceTypeAnnotationKey = "kubevirt.io/flavor"
)

// Must match providerIDs built by cloudprovider.GetInstanceProviderID
var providerIDRegexp = regexp.MustCompile(`^` + providerName + `://(\w+)$`)

type instances struct {
	cloud     *cloud
	namespace string
}

// NodeAddresses returns the addresses of the specified instance.
// TODO(roberthbailey): This currently is only used in such a way that it
// returns the address of the calling instance. We should do a rename to
// make this clearer.
func (i *instances) NodeAddresses(ctx context.Context, name types.NodeName) ([]corev1.NodeAddress, error) {
	instanceID := instanceIDFromNodeName(string(name))
	return i.nodeAddressesByInstanceID(ctx, instanceID)
}

// NodeAddressesByProviderID returns the addresses of the specified instance.
// The instance is specified using the providerID of the node. The
// ProviderID is a unique identifier of the node. This will not be called
// from the node whose nodeaddresses are being queried. i.e. local metadata
// services cannot be used in this method to obtain nodeaddresses
func (i *instances) NodeAddressesByProviderID(ctx context.Context, providerID string) ([]corev1.NodeAddress, error) {
	instanceID, err := instanceIDFromProviderID(providerID)
	if err != nil {
		glog.Errorf("Failed to get instance with provider ID %s in namespace %s: %v", providerID, i.namespace, err)
		return nil, cloudprovider.InstanceNotFound
	}
	return i.nodeAddressesByInstanceID(ctx, instanceID)
}

func (i *instances) nodeAddressesByInstanceID(ctx context.Context, instanceID string) ([]corev1.NodeAddress, error) {
	vmi, err := i.cloud.kubevirt.VirtualMachineInstance(i.namespace).Get(instanceID, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	addresses := []corev1.NodeAddress{}

	if vmi.Spec.Hostname != "" {
		v1helper.AddToNodeAddresses(&addresses, corev1.NodeAddress{
			Type:    corev1.NodeHostName,
			Address: vmi.Spec.Hostname,
		})
	} else {
		v1helper.AddToNodeAddresses(&addresses, corev1.NodeAddress{
			Type:    corev1.NodeHostName,
			Address: vmi.ObjectMeta.Name,
		})
	}

	for _, netIface := range vmi.Status.Interfaces {
		// TODO(dgonzalez): We currently assume that all IPs assigned to interfaces
		// are internal IP addresses. In the future this function must be extended
		// to detect the type of the address properly.
		v1helper.AddToNodeAddresses(&addresses, corev1.NodeAddress{
			Type:    corev1.NodeInternalIP,
			Address: netIface.IP,
		})
	}
	return addresses, nil
}

// ExternalID returns the cloud provider ID of the node with the specified NodeName.
// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
func (i *instances) ExternalID(ctx context.Context, nodeName types.NodeName) (string, error) {
	// ExternalID is deprecated in newer k8s versions in favor of InstanceID.
	return i.InstanceID(ctx, nodeName)
}

// InstanceID returns the cloud provider ID of the node with the specified NodeName.
// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
func (i *instances) InstanceID(ctx context.Context, nodeName types.NodeName) (string, error) {
	name := instanceIDFromNodeName(string(nodeName))
	vmi, err := i.cloud.kubevirt.VirtualMachineInstance(i.namespace).Get(name, &metav1.GetOptions{})
	if err != nil {
		glog.Errorf("Failed to get instance with name %s in namespace %s: %v", name, i.namespace, err)
		return "", cloudprovider.InstanceNotFound
	}

	switch vmi.Status.Phase {
	case kubevirtv1.Succeeded,
		kubevirtv1.Failed:
		glog.Infof("instance %s is shut down.", name)
		return "", cloudprovider.InstanceNotFound
	case kubevirtv1.Unknown:
		glog.Infof("instance %s is in an unkown state (host probably down).", name)
		return "", cloudprovider.InstanceNotFound
	}
	return vmi.ObjectMeta.Name, nil
}

// InstanceType returns the type of the specified instance.
func (i *instances) InstanceType(ctx context.Context, name types.NodeName) (string, error) {
	instanceID := instanceIDFromNodeName(string(name))
	return i.instanceTypeByInstanceID(ctx, instanceID)
}

// InstanceTypeByProviderID returns the type of the specified instance.
func (i *instances) InstanceTypeByProviderID(ctx context.Context, providerID string) (string, error) {
	instanceID, err := instanceIDFromProviderID(providerID)
	if err != nil {
		glog.Errorf("Failed to get instance with provider ID %s in namespace %s: %v", providerID, i.namespace, err)
		return "", cloudprovider.InstanceNotFound
	}
	return i.instanceTypeByInstanceID(ctx, instanceID)
}

func (i *instances) instanceTypeByInstanceID(ctx context.Context, instanceID string) (string, error) {
	vmi, err := i.cloud.kubevirt.VirtualMachineInstance(i.namespace).Get(instanceID, &metav1.GetOptions{})
	if err != nil {
		glog.Errorf("Failed to get instance with instance ID %s in namespace %s: %v", instanceID, i.namespace, err)
		return "", cloudprovider.InstanceNotFound
	}

	// If a type annotation is set on this VMI, return it as instance type.
	if value, ok := vmi.ObjectMeta.Annotations[instanceTypeAnnotationKey]; ok {
		return value, nil
	}
	return "", nil
}

// AddSSHKeyToAllInstances adds an SSH public key as a legal identity for all instances
// expected format for the key is standard ssh-keygen format: <protocol> <blob>
func (i *instances) AddSSHKeyToAllInstances(ctx context.Context, user string, keyData []byte) error {
	return cloudprovider.NotImplemented
}

// CurrentNodeName returns the name of the node we are currently running on
// On most clouds (e.g. GCE) this is the hostname, so we provide the hostname
func (i *instances) CurrentNodeName(ctx context.Context, hostname string) (types.NodeName, error) {
	vmis, err := i.cloud.kubevirt.VirtualMachineInstance(i.namespace).List(&metav1.ListOptions{})
	if err != nil {
		glog.Errorf("Failed to list instances in namespace %s: %v", i.namespace, err)
		return "", cloudprovider.InstanceNotFound
	}

	for _, vmi := range vmis.Items {
		if vmi.Spec.Hostname == hostname {
			return types.NodeName(vmi.ObjectMeta.Name), nil
		}
	}
	glog.Errorf("Failed to find node name for host %s", hostname)
	return "", cloudprovider.InstanceNotFound
}

// InstanceExistsByProviderID returns true if the instance for the given provider id still is running.
// If false is returned with no error, the instance will be immediately deleted by the cloud controller manager.
func (i *instances) InstanceExistsByProviderID(ctx context.Context, providerID string) (bool, error) {
	instanceID, err := instanceIDFromProviderID(providerID)
	if err != nil {
		glog.Errorf("Failed to get instance with provider ID %s in namespace %s: %v", providerID, i.namespace, err)
		return false, cloudprovider.InstanceNotFound
	}
	// If we can not get the VMI by its providerID, assume it no longer exists
	_, err = i.cloud.kubevirt.VirtualMachineInstance(i.namespace).Get(instanceID, &metav1.GetOptions{})
	if err != nil {
		glog.Errorf("Failed to get instance with provider ID %s in namespace %s: %v", providerID, i.namespace, err)
		return false, cloudprovider.InstanceNotFound
	}
	return true, nil
}

// InstanceShutdownByProviderID returns true if the instance is shutdown in cloudprovider
func (i *instances) InstanceShutdownByProviderID(ctx context.Context, providerID string) (bool, error) {
	instanceID, err := instanceIDFromProviderID(providerID)
	if err != nil {
		glog.Errorf("Failed to get instance with provider ID %s in namespace %s: %v", providerID, i.namespace, err)
		return true, cloudprovider.InstanceNotFound
	}
	vmi, err := i.cloud.kubevirt.VirtualMachineInstance(i.namespace).Get(instanceID, &metav1.GetOptions{})
	if err != nil {
		glog.Errorf("Failed to get instance with provider ID %s in namespace %s: %v", providerID, i.namespace, err)
		return true, cloudprovider.InstanceNotFound
	}

	switch vmi.Status.Phase {
	case kubevirtv1.Succeeded,
		kubevirtv1.Failed:
		return true, nil
	case kubevirtv1.Unknown:
		return true, fmt.Errorf("Instance is in unkown state (propably host down)")
	}
	return false, nil
}

// instanceIDFromNodeName extracts the instance ID from a given node name. In
// case the node name is a FQDN the hostname will be extracted as instance ID.
func instanceIDFromNodeName(nodeName string) string {
	data := strings.SplitN(nodeName, ".", 2)
	return data[0]
}

// instanceIDFromProviderID extracts the instance ID from a provider ID.
func instanceIDFromProviderID(providerID string) (instanceID string, err error) {
	matches := providerIDRegexp.FindStringSubmatch(providerID)
	if len(matches) != 2 {
		return "", fmt.Errorf("ProviderID \"%s\" didn't match expected format \"%s://InstanceID\"", providerID, providerName)
	}
	return matches[1], nil
}