package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"crypto/sha256"
	"encoding/base32"

	"qiniu.com/qvirt/apis/qvm/v1alpha1"
	qvirtinformers "qiniu.com/qvirt/client/informers/externalversions"
	qvirtlister "qiniu.com/qvirt/client/listers/qvm/v1alpha1"
	"qiniu.com/qvirt/client/versioned"
	iptableutil "qiniu.com/qvirt/pkg/iptables"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/exec"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	// the qvirt mapping chain
	qvirtMappingChain iptableutil.Chain = "QVIRT-MAPPING"
)

var doDelete bool

func init() {
	flag.BoolVar(&doDelete, "delete", false, "whether do cleanup IPTable rules")
}

type Proxy struct {
	QvmLister  qvirtlister.QvmLister
	NodeGetter func(name string) (*corev1.Node, error)

	iptables       iptableutil.Interface
	InformerSynced cache.InformerSynced
}

func main() {
	flag.Parse()
	ctx := controllerruntime.SetupSignalHandler()
	var pxy = &Proxy{iptables: iptableutil.New(exec.New(), iptableutil.ProtocolIPv4)}

	if doDelete {
		log.Println("deleting IPTable rules...")
		encounteredError := pxy.CleanupLeftovers()
		if encounteredError {
			log.Println("cleanup IPTable rules failed")
		} else {
			log.Println("cleanup IPTable rules success")
		}
		return
	}

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

	qvmInf.AddEventHandler(&cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pxy.SyncRules()
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			pxy.SyncRules()
		},
		DeleteFunc: func(obj interface{}) {
			pxy.SyncRules()
		},
	})

	go qvmInf.Run(ctx.Done())
	go pxy.Run(ctx.Done())
	<-ctx.Done()
}

func (p *Proxy) Run(stopCh <-chan struct{}) {
	if !cache.WaitForNamedCacheSync("qvm config", stopCh, p.InformerSynced) {
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
		var internalPodIP string
		var externalIPs = vm.Spec.FloatingIPs

		if len(vm.Status.Network.Interfaces) > 0 {
			internalPodIP = vm.Status.Network.Interfaces[0].IP
		}

		if internalPodIP == "" {
			log.Printf("unexpected, vm %s/%s don't have IP address", vm.Namespace, vm.Name)
			continue
		}

		for _, eip := range externalIPs {
			mpChain := ipMappingChain(eip, internalPodIP)
			ok, err := p.iptables.EnsureChain(iptableutil.TableNAT, mpChain)
			log.Printf("ensure DNAT rule/%s for vm %s/%s, ok: %v, err: %v\n", mpChain, vm.Namespace, vm.Name, ok, err)

			log.Printf("ensure nat rules for vm %s/%s\n", vm.Namespace, vm.Name)
			log.Printf("firstly set KUBE-MARK-MASK for SNAT, %s/%s\n", vm.Namespace, vm.Name)
			ok, err = p.iptables.EnsureRule(iptableutil.Append, iptableutil.TableNAT, qvirtMappingChain,
				"-m", "comment", "--comment", fmt.Sprintf("%s/%s external IP", vm.Namespace, vm.Name),
				"-d", net.ParseIP(eip).String(),
				"!", "-s", "10.233.64.0/18",
				"-j", "KUBE-MARK-MASQ",
			)
			log.Printf("ret for set KUBE-MARK-MASK for %s/%s, ok: %v, err: %v\n", vm.Namespace, vm.Name, ok, err)
			log.Printf("set jump QVIRT-MP-XXX for DNAT %s/%s\n", vm.Namespace, vm.Name)
			ok, err = p.iptables.EnsureRule(iptableutil.Append, iptableutil.TableNAT, qvirtMappingChain,
				"-m", "comment", "--comment", fmt.Sprintf("%s/%s external IP", vm.Namespace, vm.Name),
				"-d", net.ParseIP(eip).String(),
				"-j", string(mpChain),
			)
			log.Printf("ret for %s to pod/%s, ok:%v, err:%v", eip, internalPodIP, ok, err)
			log.Printf("try add DNAT from %s to %s...\n", eip, internalPodIP)
			ok, err = p.iptables.EnsureRule(iptableutil.Append, iptableutil.TableNAT, mpChain,
				"-j", "DNAT", "--to-destination", net.ParseIP(internalPodIP).String(),
			)
			log.Printf("ret for DNAT %s to pod/%s, ok:%v, err:%v", eip, internalPodIP, ok, err)
		}
	}
}

func (p *Proxy) CleanupLeftovers() (encounteredError bool) {
	var err error
	for _, jump := range iptablesJumpChains {
		args := append(jump.extraArgs,
			"-m", "comment", "--comment", jump.comment,
			"-j", string(jump.dstChain),
		)
		if err = p.iptables.DeleteRule(jump.table, jump.srcChain, args...); err != nil {
			if !iptableutil.IsNotFoundError(err) {
				log.Printf("Error removing pure-iptables proxy rule: %v", err)
				encounteredError = true
			}
		}
	}

	// Flush and remove all of our "-t nat" chains.
	iptablesData := bytes.NewBuffer(nil)
	if err := p.iptables.SaveInto(iptableutil.TableNAT, iptablesData); err != nil {
		klog.ErrorS(err, "Failed to execute iptables-save", "table", iptableutil.TableNAT)
		encounteredError = true
	} else {
		existingNATChains := iptableutil.GetChainLines(iptableutil.TableNAT, iptablesData.Bytes())
		if _, found := existingNATChains[qvirtMappingChain]; found {
			err = p.iptables.FlushChain(iptableutil.TableNAT, qvirtMappingChain)
			log.Println(err)
			err = p.iptables.DeleteChain(iptableutil.TableNAT, qvirtMappingChain)
			log.Println(err)
		}

		// Hunt for QVIRT-MP-XXX chains.
		for chain := range existingNATChains {
			chainString := string(chain)
			if strings.HasPrefix(chainString, ipMappingChainPrefix) {
				err = p.iptables.FlushChain(iptableutil.TableNAT, chain)
				log.Println(err)
				err = p.iptables.DeleteChain(iptableutil.TableNAT, chain)
				log.Println(err)
			}
		}
	}
	return
}
