package main

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"log"
	"net"
	"time"

	"qiniu.com/qvirt/apis/qvm/v1alpha1"
	qvirtinformers "qiniu.com/qvirt/client/informers/externalversions"
	qvirtlister "qiniu.com/qvirt/client/listers/qvm/v1alpha1"
	"qiniu.com/qvirt/client/versioned"
	iptableutil "qiniu.com/qvirt/pkg/iptables"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/exec"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	// the qvirt mapping chain
	qvirtMappingChain iptableutil.Chain = "QVIRT-MAPPING"
)

type Proxy struct {
	QvmLister  qvirtlister.QvmLister
	NodeGetter func(name string) (*corev1.Node, error)

	iptables       iptableutil.Interface
	InformerSynced cache.InformerSynced
}

func main() {
	ctx := controllerruntime.SetupSignalHandler()
	var pxy = &Proxy{}

	kubeconfig := config.GetConfigOrDie()

	vcli := versioned.NewForConfigOrDie(kubeconfig)
	kcli := kubernetes.NewForConfigOrDie(kubeconfig)

	sharedInformer := qvirtinformers.NewSharedInformerFactory(vcli, 30*time.Second)
	qvmInf := sharedInformer.Qvm().V1alpha1().Qvms().Informer()
	pxy.QvmLister = sharedInformer.Qvm().V1alpha1().Qvms().Lister()
	pxy.InformerSynced = qvmInf.HasSynced

	pxy.NodeGetter = func(name string) (*corev1.Node, error) {
		return kcli.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	}

	qvmInf.AddEventHandler(&cache.FilteringResourceEventHandler{
		FilterFunc: nil,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pxy.SyncRules()
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				pxy.SyncRules()
			},
			DeleteFunc: func(obj interface{}) {
				pxy.SyncRules()
			},
		},
	})

	execer := exec.New()
	pxy.iptables = iptableutil.New(execer, iptableutil.ProtocolIPv4)

	go pxy.Run(ctx.Done())
	<-ctx.Done()
}

func (p *Proxy) Run(stopCh <-chan struct{}) {
	if !cache.WaitForNamedCacheSync("service config", stopCh, p.InformerSynced) {
		return
	}
	p.SyncRules()
}

func (p *Proxy) SyncRules() {
	// Create and link the kube chains.
	for _, jump := range iptablesJumpChains {
		if _, err := p.iptables.EnsureChain(jump.table, jump.dstChain); err != nil {
			log.Printf("Failed to ensure chain exists, table %s, chain %s: %v", jump.table, jump.dstChain, err)
			return
		}
		args := append(jump.extraArgs,
			"-m", "comment", "--comment", jump.comment,
			"-j", string(jump.dstChain),
		)
		if _, err := p.iptables.EnsureRule(iptableutil.Prepend, jump.table, jump.srcChain, args...); err != nil {
			klog.ErrorS(err, "Failed to ensure chain jumps", "table", jump.table, "srcChain", jump.srcChain, "dstChain", jump.dstChain)
			return
		}
	}
	p.syncIngressNAT()
}

type iptablesJumpChain struct {
	table     iptableutil.Table
	dstChain  iptableutil.Chain
	srcChain  iptableutil.Chain
	comment   string
	extraArgs []string
}

var iptablesJumpChains = []iptablesJumpChain{
	{iptableutil.TableNAT, qvirtMappingChain, iptableutil.ChainOutput, "qvirt floating ip mapping portals", nil},
	{iptableutil.TableNAT, qvirtMappingChain, iptableutil.ChainPrerouting, "qvirt floating portals", nil},
}

const ipMappingChainPrefix = "QVIRT-MP-"

func ipMappingChain(externalIp, internalIp string) iptableutil.Chain {
	return iptableutil.Chain(ipMappingChainPrefix + ipHash(externalIp, internalIp))
}

func ipHash(externalIp, internalIp string) string {
	hash := sha256.Sum256([]byte(externalIp + internalIp))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return encoded[:16]
}

func (p *Proxy) syncIngressNAT() {
	qvmList, err := p.QvmLister.List(labels.Everything())
	if err != nil {
		log.Println("err list qvms", err)
		return
	}

	//todo can we do it at informer level?
	var targets []*v1alpha1.Qvm
	for _, vm := range qvmList {
		if len(vm.Spec.FloatingIPs) > 0 && vm.Status.Phase == "Running" && vm.Status.NodeName != "" {
			targets = append(targets, vm)
		}
	}

	for _, vm := range targets {
		var internalNodeIP string
		var externalIPs = vm.Spec.FloatingIPs

		node, err := p.NodeGetter(vm.Status.NodeName)
		if err != nil {
			log.Printf("err get node %s: %v", node, err)
			continue
		}
		for i := range node.Status.Addresses {
			if node.Status.Addresses[i].Type == "InternalIP" {
				internalNodeIP = node.Status.Addresses[i].Address
				break
			}
		}
		if internalNodeIP == "" {
			log.Printf("unexpected node %s don't have internal IP address", node)
			continue
		}

		for _, eip := range externalIPs {
			mpChain := ipMappingChain(eip, internalNodeIP)
			log.Printf("ensure nat rules for vm %s/%s\n", vm.Namespace, vm.Name)
			p.iptables.EnsureRule(iptableutil.Append, iptableutil.TableNAT, qvirtMappingChain,
				"-m", "comment", "--comment", fmt.Sprintf(`"%s/%s external IP"`, vm.Namespace, vm.Name),
				"-d", net.ParseIP(eip).String(),
				"-j", string(mpChain),
			)
			p.iptables.EnsureRule(iptableutil.Append, iptableutil.TableNAT, mpChain,
				"-j", "DNAT", "--to-destination", net.ParseIP(internalNodeIP).String(),
			)
		}
	}
}
