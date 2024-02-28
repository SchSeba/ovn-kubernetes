package persistentips

import (
	"errors"
	"fmt"
	ipam "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"k8s.io/klog/v2"
	"net"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	ipamclaimslister "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/listers/ipamclaims/v1alpha1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovnktypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	ErrPersistentIPsNotAvailableOnNetwork = errors.New("ipam claims not supported on this network")
)

type IPReleaser interface {
	ReleaseIPs(ips []*net.IPNet) error
}

type IPAllocator interface {
	AllocateIPs(ips []*net.IPNet) error
}

type PersistentAllocations interface {
	FindIPAMClaim(claimName string, namespace string) (*ipamclaimsapi.IPAMClaim, error)

	Reconcile(oldIPAMClaim *ipamclaimsapi.IPAMClaim, newIPAMClaim *ipamclaimsapi.IPAMClaim, ipReleaser IPReleaser) error
}

// IPAMClaimReconciler acts on IPAMClaim events handed off by the cluster network
// controller and allocates or releases IPs for IPAMClaims.
type IPAMClaimReconciler struct {
	kube kube.InterfaceOVN

	// netInfo is used to filter relevant IPAMClaim events when syncing
	// i.e. each NetworkController has a PersistentIPs.IPAMClaimReconciler, which syncs
	// and deletes IPAM claims for a *single* network
	// we need this to know if the network supports IPAM
	netInfo util.NetInfo

	lister ipamclaimslister.IPAMClaimLister
}

// NewIPAMClaimReconciler builds a new PersistentIPsAllocator
func NewIPAMClaimReconciler(kube kube.InterfaceOVN, netConfig util.NetInfo, lister ipamclaimslister.IPAMClaimLister) *IPAMClaimReconciler {
	pipsAllocator := &IPAMClaimReconciler{
		kube:    kube,
		netInfo: netConfig,
		lister:  lister,
	}
	return pipsAllocator
}

// Reconcile updates an IPAMClaim with the IP addresses allocated to the pod's
// interface
func (icr *IPAMClaimReconciler) Reconcile(
	oldIPAMClaim *ipamclaimsapi.IPAMClaim,
	newIPAMClaim *ipamclaimsapi.IPAMClaim,
	ipReleaser IPReleaser,
) error {
	var ipamClaim *ipamclaimsapi.IPAMClaim
	if oldIPAMClaim != nil {
		ipamClaim = oldIPAMClaim
	}
	if newIPAMClaim != nil {
		ipamClaim = newIPAMClaim
	}

	if ipamClaim == nil {
		return nil
	}

	mustUpdateIPAMClaim := (oldIPAMClaim == nil ||
		len(oldIPAMClaim.Status.IPs) == 0) &&
		newIPAMClaim != nil

	if mustUpdateIPAMClaim {
		if err := icr.kube.UpdateIPAMClaimIPs(newIPAMClaim); err != nil {
			return fmt.Errorf(
				"failed to update the allocation %q with allocations %q: %w",
				newIPAMClaim.Name,
				newIPAMClaim.Status.IPs,
				err,
			)
		}
		return nil
	}

	var originalIPs []string
	if len(oldIPAMClaim.Status.IPs) > 0 {
		originalIPs = oldIPAMClaim.Status.IPs
	}

	var newIPs []string
	if newIPAMClaim != nil && len(newIPAMClaim.Status.IPs) > 0 {
		newIPs = newIPAMClaim.Status.IPs
	}

	areClaimsEqual := cmp.Equal(
		originalIPs,
		newIPs,
		cmpopts.SortSlices(func(a, b string) bool { return a < b }),
	)

	if !areClaimsEqual {
		ipamClaimKey := fmt.Sprintf("%s/%s", ipamClaim.Namespace, ipamClaim.Name)
		return fmt.Errorf(
			"failed to update IPAMClaim %q - overwriting existing IPs %q with newer IPs %q",
			ipamClaimKey,
			originalIPs,
			newIPs,
		)
	}

	return nil
}

func (icr *IPAMClaimReconciler) FindIPAMClaim(claimName string, namespace string) (*ipamclaimsapi.IPAMClaim, error) {
	if icr.lister == nil ||
		!util.DoesNetworkRequireIPAM(icr.netInfo) ||
		icr.netInfo.TopologyType() == ovnktypes.Layer3Topology ||
		claimName == "" {
		return nil, ErrPersistentIPsNotAvailableOnNetwork
	}
	claim, err := icr.lister.IPAMClaims(namespace).Get(claimName)
	if err != nil {
		return nil, fmt.Errorf("failed to get IPAMClaim %q: %w", claimName, err)
	}
	return claim, nil
}

// Sync initializes the IPs allocator with the IPAMClaims already existing on
// the cluster. For live pods, therse are already allocated, so no error will
// be thrown (e.g. we ignore the `ipam.IsErrAllocated` error
func (icr *IPAMClaimReconciler) Sync(objs []interface{}, ipAllocator IPAllocator) error {
	var ips []*net.IPNet
	for _, obj := range objs {
		ipamClaim, ok := obj.(*ipamclaimsapi.IPAMClaim)
		if !ok {
			klog.Errorf("Could not cast %T object to *ipamclaimsapi.IPAMClaim", obj)
			continue
		}
		if ipamClaim.Spec.Network != icr.netInfo.GetNetworkName() {
			klog.V(5).Infof(
				"Ignoring IPAMClaim for network %q in controller: %s",
				ipamClaim.Spec.Network,
				icr.netInfo.GetNetworkName(),
			)
			continue
		}
		ipnets, err := util.ParseIPNets(ipamClaim.Status.IPs)
		if err != nil {
			return fmt.Errorf("failed at parsing IP when allocating persistent IPs: %w", err)
		}
		ips = append(ips, ipnets...)
	}
	if len(ips) > 0 {
		if err := ipAllocator.AllocateIPs(ips); err != nil && !ipam.IsErrAllocated(err) {
			return fmt.Errorf("failed allocating persistent ips: %w", err)
		}
	}
	return nil
}
