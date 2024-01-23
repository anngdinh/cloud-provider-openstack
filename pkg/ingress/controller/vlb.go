package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	"github.com/sirupsen/logrus"
	"github.com/vngcloud/vngcloud-go-sdk/client"
	vconSdkClient "github.com/vngcloud/vngcloud-go-sdk/client"
	"github.com/vngcloud/vngcloud-go-sdk/vngcloud"
	lObjects "github.com/vngcloud/vngcloud-go-sdk/vngcloud/objects"
	"github.com/vngcloud/vngcloud-go-sdk/vngcloud/services/identity/v2/extensions/oauth2"
	"github.com/vngcloud/vngcloud-go-sdk/vngcloud/services/identity/v2/tokens"
	"github.com/vngcloud/vngcloud-go-sdk/vngcloud/services/loadbalancer/v2/listener"
	"github.com/vngcloud/vngcloud-go-sdk/vngcloud/services/loadbalancer/v2/loadbalancer"
	"github.com/vngcloud/vngcloud-go-sdk/vngcloud/services/loadbalancer/v2/policy"
	"github.com/vngcloud/vngcloud-go-sdk/vngcloud/services/loadbalancer/v2/pool"
	apiv1 "k8s.io/api/core/v1"
	nwv1 "k8s.io/api/networking/v1"
	"k8s.io/cloud-provider-openstack/pkg/ingress/config"
	"k8s.io/cloud-provider-openstack/pkg/ingress/consts"
	"k8s.io/cloud-provider-openstack/pkg/ingress/utils/errors"
	"k8s.io/cloud-provider-openstack/pkg/ingress/utils/metadata"
	"k8s.io/klog/v2"
)

type (
	ExtraInfo struct {
		ProjectID string
		UserID    int64
	}
)

//type ILBProvider interface {
//	Init() error
//
//	// GetLoadbalancerByID returns the load balancer with the given name (list all lb in subnet and filter by name)
//	GetLoadbalancerByID(lbID string) (*loadbalancers.LoadBalancer, error)
//
//	// Update lb memebers when node change
//	UpdateLoadbalancerMembers(lbID string, nodes []*apiv1.Node) error
//
//	// GetLoadbalancerIDByIngress returns the load balancer id with the given ingress
//	GetLoadbalancerIDByIngress(ing *nwv1.Ingress) string
//
//	EnsureFloatingIP(needDelete bool, portID string, floatingIPNetwork string, description string) (string, error)
//	DeleteLoadbalancer(lbID string, cascade bool) error
//
//	// EnsureLoadBalancer creates a new load balancer or updates an existing one.
//	EnsureLoadBalancer(con *Controller, ing *nwv1.Ingress) (*loadbalancers.LoadBalancer, error)
//	EnsureListener(name string, lbID string, secretRefs []string, listenerAllowedCIDRs []string, timeoutClientData, timeoutMemberData, timeoutTCPInspect, timeoutMemberConnect *int) (*listeners.Listener, error)
//
//	GetL7policies(listenerID string) ([]l7policies.L7Policy, error)
//	GetL7Rules(policyID string) ([]l7policies.Rule, error)
//	GetPools(lbID string) ([]pools.Pool, error)
//
//	// UpdateLoadBalancerDescription updates the load balancer description field.
//	UpdateLoadBalancerDescription(lbID string, newDescription string) error
//}

type VLBProvider struct {
	config *config.Config

	provider  *vconSdkClient.ProviderClient
	vLBSC     *client.ServiceClient
	vServerSC *client.ServiceClient

	cluster     *lObjects.Cluster
	lbsInSubnet []*lObjects.LoadBalancer

	extraInfo    *ExtraInfo
	metadataOpts metadata.Opts
	api          API
}

func (c *VLBProvider) Init() error {
	c.api = API{}
	provider, err := vngcloud.NewClient(c.config.Global.IdentityURL)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Fatal("failed to init VNGCLOUD client")
	}
	err = vngcloud.Authenticate(provider, &oauth2.AuthOptions{
		ClientID:     c.config.Global.ClientID,
		ClientSecret: c.config.Global.ClientSecret,
		AuthOptionsBuilder: &tokens.AuthOptions{
			IdentityEndpoint: c.config.Global.IdentityURL,
		},
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Fatal("failed to Authenticate VNGCLOUD client")
	}
	c.provider = provider

	vlbSC, err := vngcloud.NewServiceClient(
		"https://hcm-3.api.vngcloud.vn/vserver/vlb-gateway/v2",
		provider, "vlb-gateway")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Fatal("failed to init VLB VNGCLOUD client")
	}
	c.vLBSC = vlbSC

	vserverSC, err := vngcloud.NewServiceClient(
		"https://hcm-3.api.vngcloud.vn/vserver/vserver-gateway/v2",
		provider, "vserver-gateway")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Fatal("failed to init VSERVER VNGCLOUD client")
	}
	c.vServerSC = vserverSC

	c.setUpPortalInfo()
	c.cluster, err = c.api.GetClusterInfo(c.vServerSC, c.extraInfo.ProjectID, c.config.ClusterID)
	c.ListLoadBalancerBySubnetID()

	return nil
}

func (c *VLBProvider) GetLoadbalancerByID(lbID string) (*loadbalancers.LoadBalancer, error) {
	c.ListLoadBalancerBySubnetID()

	for _, lb := range c.lbsInSubnet {
		if lb.UUID == lbID {
			return &loadbalancers.LoadBalancer{
				ID:              lb.UUID,
				VipAddress:      lb.Address,
				Name:            lb.Name,
				OperatingStatus: lb.Status,
			}, nil
		}
	}
	return nil, nil
}

func (c *VLBProvider) UpdateLoadbalancerMembers(lbID string, nodes []*apiv1.Node) error {
	// for every pools, except the default pool, update the members with the new nodes id
	// ..........................................................
	// how to find the default pool?
	return nil
}

func (c *VLBProvider) GetLoadbalancerIDByIngress(ing *nwv1.Ingress) (string, error) {
	klog.Infof("----------------- GetLoadbalancerIDByIngress(%s/%s) ------------------", ing.Namespace, ing.Name)
	c.ListLoadBalancerBySubnetID()
	// check in annotation
	if lbID, ok := ing.Annotations[ServiceAnnotationLoadBalancerID]; ok {
		logrus.Infof("have annotation lbID: %s", lbID)
		for _, lb := range c.lbsInSubnet {
			if lb.UUID == lbID {
				logrus.Infof("found lbID: %s", lbID)
				return lb.UUID, nil
			}
		}
		logrus.Infof("have annotation but not found lbID: %s", lbID)
		return "", errors.ErrLoadBalancerIDNotFoundAnnotation
	}

	// check in list lb name
	lbName := c.GetResourceName(ing)
	for _, lb := range c.lbsInSubnet {
		if lb.Name == lbName {
			logrus.Infof("Found lb match Name: %s", lbName)
			return lb.UUID, nil
		}
	}
	logrus.Infof("Not found lb match Name: %s", lbName)
	return "", nil
}

func (c *VLBProvider) DeleteLoadbalancer(con *Controller, ing *nwv1.Ingress) error {
	klog.Infof("----------------- DeleteLoadbalancer(%s/%s) ------------------", ing.Namespace, ing.Name)
	lbID, err := c.GetLoadbalancerIDByIngress(ing)
	if err != nil {
		if err == errors.ErrLoadBalancerIDNotFoundAnnotation {
			logrus.Infof("Not found lbID in annotation, maybe already deleted!")
			return nil
		}

		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Fatal("error not handled when list loadbalancer by subnet id")
	}
	if lbID == "" {
		logrus.Infof("Not found lbID, maybe already deleted!")
		return nil
	}
	lb := c.WaitForLBActive(lbID)

	// Delete l7 load balancing rules. Each rule is listener, each path is policy, each service is pool
	for ruleIndex, _ := range ing.Spec.Rules {
		listenerName := fmt.Sprintf("%s_l%d", lb.Name, ruleIndex)
		lis, err := c.FindListenerByName(lb.UUID, listenerName)
		if err != nil {
			logrus.Errorln("error when find listener by name", err)
			return err
		}
		logrus.Infof("listener: %v", lis)

		policyArr, err := c.api.ListPolicyOfListener(c.vLBSC, c.extraInfo.ProjectID, lb.UUID, lis.ID)
		if err != nil {
			logrus.Errorln("error when list policy", err)
			return err
		}

		poolIDArr := make([]string, 0)
		for _, pol := range policyArr {
			if pol.Action == string(policy.PolicyOptsActionOptREDIRECTTOPOOL) {
				poolIDArr = append(poolIDArr, pol.RedirectPoolID)
			}
		}

		err = c.api.DeleteListener(c.vLBSC, c.extraInfo.ProjectID, lb.UUID, lis.ID)
		if err != nil {
			logrus.Errorln("error when delete listener", err)
			return err
		}
		c.WaitForLBActive(lbID)

		for _, poolID := range poolIDArr {
			err := c.api.DeletePool(c.vLBSC, c.extraInfo.ProjectID, lb.UUID, poolID)
			if err != nil {
				logrus.Errorln("error when delete pool", err)
				return err
			}
			c.WaitForLBActive(lbID)
		}

	}
	return nil
}

func (c *VLBProvider) EnsureLoadBalancer(con *Controller, oldIng, ing *nwv1.Ingress) (*lObjects.LoadBalancer, error) {
	klog.Infof("----------------- EnsureLoadBalancer(%s/%s) ------------------", ing.Namespace, ing.Name)
	lbID, err := c.GetLoadbalancerIDByIngress(ing)
	lb_prefix_name := c.GetResourceName(ing)
	if err != nil {
		if err == errors.ErrLoadBalancerIDNotFoundAnnotation {
			return nil, err
		}

		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Fatal("error not handled when list loadbalancer by subnet id")
	}

	if lbID == "" {
		klog.Infof("--------------- create new lb for ingress %s/%s -------------------", ing.Namespace, ing.Name)
		lbName := c.GetResourceName(ing)
		packageID := getStringFromIngressAnnotation(ing, ServiceAnnotationPackageID, consts.DEFAULT_PACKAGE_ID)

		lb, err := c.api.CreateLB(c.vLBSC,
			lbName, packageID, c.cluster.SubnetID, c.extraInfo.ProjectID,
			loadbalancer.CreateOptsSchemeOptInternet,
			loadbalancer.CreateOptsTypeOptLayer7)
		if err != nil {
			klog.Errorf("error when create new lb: %v", err)
			return nil, err
		}
		lbID = lb.UUID
	}
	lb := c.WaitForLBActive(lbID)

	// default pool
	// add default backend to it if specified .......................................................
	defaultPool, err := c.ensurePool(lb.UUID, consts.DEFAULT_NAME_DEFAULT_POOL)
	if err != nil {
		logrus.Errorln("error when ensure default pool", err)
		return nil, err
	}
	logrus.Infof("default pool: %v", defaultPool)

	// Add l7 load balancing rules. Only have 2 listener: http and https
	for ruleIndex, rule := range ing.Spec.Rules {
		isHttpsListener := false // ...........................................
		listenerOpts := consts.OPT_LISTENER_HTTP_DEFAULT
		if isHttpsListener {
			listenerOpts = consts.OPT_LISTENER_HTTPS_DEFAULT
		}
		listenerOpts.DefaultPoolId = defaultPool.UUID

		lis, err := c.ensureListener(lb.UUID, listenerOpts)
		if err != nil {
			logrus.Errorln("error when ensure listener:", listenerOpts.ListenerName, err)
			return nil, err
		}
		logrus.Infof("listener: %v", lis)

		for pathIndex, path := range rule.HTTP.Paths {
			poolName := fmt.Sprintf("%s_r%d_p%d", lb_prefix_name, ruleIndex, pathIndex)

			serviceName := fmt.Sprintf("%s/%s", ing.ObjectMeta.Namespace, path.Backend.Service.Name)
			klog.Infof("serviceName: %v", serviceName)
			nodePort, err := con.getServiceNodePort(serviceName, path.Backend.Service)
			if err != nil {
				klog.Errorf("error when get node port: %v", err)
				return nil, err
			}
			klog.Infof("nodePort: %v", nodePort)

			membersAddr, _ := con.GetNodeMembersAddr()
			klog.Infof("membersAddr: %v", membersAddr)
			members := make([]pool.Member, 0)
			for _, addr := range membersAddr {
				members = append(members, pool.Member{
					IpAddress:   addr,
					Port:        nodePort,
					Backup:      false,
					Weight:      1,
					Name:        addr,
					MonitorPort: nodePort,
				})
			}

			newPool, err := c.ensurePool(lb.UUID, poolName)
			if err != nil {
				logrus.Errorln("error when ensure pool", err)
				return nil, err
			}
			logrus.Infof("pool: %v", newPool)
			_, err = c.ensurePoolMember(lb.UUID, newPool.UUID, members)
			if err != nil {
				logrus.Errorln("error when ensure pool member", err)
				return nil, err
			}

			// create policy
			policyOpts := &policy.CreateOptsBuilder{
				Name:           poolName,
				Action:         policy.PolicyOptsActionOptREDIRECTTOPOOL,
				RedirectPoolID: newPool.UUID,
				Rules: []policy.Rule{
					{
						RuleType:    policy.PolicyOptsRuleTypeOptHOSTNAME,
						CompareType: policy.PolicyOptsCompareTypeOptEQUALS,
						RuleValue:   rule.Host,
					},
					{
						RuleType:    policy.PolicyOptsRuleTypeOptPATH,
						CompareType: policy.PolicyOptsCompareTypeOptEQUALS,
						RuleValue:   path.Path,
					},
				},
			}
			_, err = c.ensurePolicy(lb.UUID, lis.ID, policyOpts)
			if err != nil {
				logrus.Errorln("error when ensure policy", err)
				return nil, err
			}
		}
	}

	// if oldIng != nil {
	// 	if len(oldIng.Spec.Rules) > len(ing.Spec.Rules) {
	// 		klog.Infof("--------------- delete old listener for ingress %s/%s -------------------", oldIng.Namespace, oldIng.Name)
	// 		// delete old lb
	// 		for i := len(ing.Spec.Rules); i < len(oldIng.Spec.Rules); i++ {
	// 			listenerName := fmt.Sprintf("%s_l%d", lb.Name, i)
	// 			lis, err := c.FindListenerByName(lb.UUID, listenerName)
	// 			if err != nil {
	// 				if err == errors.ErrNotFound {
	// 					logrus.Infof("listener not found: %s", listenerName)
	// 					continue
	// 				}
	// 				logrus.Errorln("error when find listener by name", err)
	// 				return nil, err
	// 			}
	// 			c.api.DeleteListener(c.vLBSC, c.extraInfo.ProjectID, lb.UUID, lis.ID)
	// 		}
	// 	}
	// }

	return lb, nil
}

// /////////////////////////////////// PRIVATE METHOD /////////////////////////////////////////

// GetResourceName get Ingress related resource name.
func (c *VLBProvider) GetResourceName(ing *nwv1.Ingress) string {
	fullName := fmt.Sprintf("%s_%s_%s", c.config.ClusterName, ing.Namespace, ing.Name)
	hash := HashString(fullName)

	MinInt := func(a, b int) int {
		if a < b {
			return a
		}
		return b
	}
	trim := func(str string, length int) string {
		return str[:MinInt(len(str), length)]
	}
	return fmt.Sprintf("annd2_%s", trim(hash, 10))
	// return fmt.Sprintf("annd2_%s_%s_%s",
	// 	trim(c.config.ClusterName, 10),
	// 	trim(ing.Name, 10),
	// 	trim(hash, 10),
	// )
}

func (c *VLBProvider) setUpPortalInfo() {
	c.config.Metadata = getMetadataOption(metadata.Opts{})
	metadator := metadata.GetMetadataProvider(c.config.Metadata.SearchOrder)
	extraInfo, err := setupPortalInfo(
		c.provider,
		metadator,
		"https://hcm-3.api.vngcloud.vn/vserver/vserver-gateway/v1")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Fatal("failed to setup portal info")
	}
	c.extraInfo = extraInfo
}

func (c *VLBProvider) ensurePool(lbID, poolName string) (*lObjects.Pool, error) {
	klog.Infof("------------ ensurePool: %s", poolName)
	pool, err := c.FindPoolByName(lbID, poolName)
	if err != nil {
		if err == errors.ErrNotFound {
			newPoolOpts := consts.OPT_POOL_DEFAULT
			newPoolOpts.PoolName = poolName
			newPool, err := c.api.CreatePool(c.vLBSC, lbID, c.extraInfo.ProjectID, newPoolOpts)
			if err != nil {
				logrus.Errorln("error when create new pool", err)
				return nil, err
			}
			pool = newPool
		} else {
			logrus.Errorln("error when find pool", err)
			return nil, err
		}
	}
	c.WaitForLBActive(lbID)
	return pool, nil
}

func (c *VLBProvider) ensurePoolMember(lbID, poolID string, members []pool.Member) (*lObjects.Pool, error) {
	klog.Infof("------------ ensurePoolMember: %s", poolID)
	memsGet, err := c.api.GetMemberPool(c.vLBSC, c.extraInfo.ProjectID, lbID, poolID)
	if err != nil {
		logrus.Errorln("error when get pool members", err)
		return nil, err
	}
	comparePoolMembers := func(p1 []pool.Member, p2 []*lObjects.Member) bool {
		if len(p1) != len(p2) {
			return false
		}
		checkIfExist := func(mems []*lObjects.Member, mem pool.Member) bool {
			for _, r := range mems {
				if r.Address == mem.IpAddress &&
					r.ProtocolPort == mem.Port &&
					r.MonitorPort == mem.MonitorPort &&
					r.Backup == mem.Backup &&
					r.Name == mem.Name &&
					r.Weight == mem.Weight {
					return true
				}
			}
			return false
		}
		for _, p := range p1 {
			if !checkIfExist(p2, p) {
				logrus.Infof("member in pool not exist: %v", p)
				return false
			}
		}
		return true
	}
	if !comparePoolMembers(members, memsGet) {
		err := c.api.UpdatePoolMember(c.vLBSC, c.extraInfo.ProjectID, lbID, poolID, members)
		if err != nil {
			logrus.Errorln("error when update pool members", err)
			return nil, err
		}
	}

	c.WaitForLBActive(lbID)
	return nil, nil
}

func (c *VLBProvider) ensureListener(lbID string, listenerOpts listener.CreateOpts) (*lObjects.Listener, error) {
	klog.Infof("------------ ensureListener ----------")
	lis, err := c.FindListenerByName(lbID, listenerOpts.ListenerName)
	if err != nil {
		if err == errors.ErrNotFound {
			// create listener point to default pool
			listener, err := c.api.CreateListener(c.vLBSC, lbID, c.extraInfo.ProjectID, &listenerOpts)
			if err != nil {
				logrus.Fatal("error when create listener", err)
				return nil, err
			}
			lis = listener
		} else {
			logrus.Errorln("error when find listener", err)
			return nil, err
		}
	}
	c.WaitForLBActive(lbID)
	return lis, nil
}

func (c *VLBProvider) ensurePolicy(lbID, listenerID string, policyOpt *policy.CreateOptsBuilder) (*lObjects.Policy, error) {
	klog.Infof("------------ ensurePolicy: %s", policyOpt.Name)
	FindPolicyByName := func() (*lObjects.Policy, error) {
		policyArr, err := c.api.ListPolicyOfListener(c.vLBSC, c.extraInfo.ProjectID, lbID, listenerID)
		if err != nil {
			logrus.Errorln("error when list policy", err)
			return nil, err
		}
		for _, policy := range policyArr {
			if policy.Name == policyOpt.Name {
				return policy, nil
			}
		}
		return nil, errors.ErrNotFound
	}

	pol, err := FindPolicyByName()
	if err != nil {
		if err == errors.ErrNotFound {
			newPolicy, err := c.api.CreatePolicy(c.vLBSC, c.extraInfo.ProjectID, lbID, listenerID, policyOpt)
			if err != nil {
				logrus.Fatal("error when create polipolicyNamecy", err)
				return nil, err
			}
			pol = newPolicy
		} else {
			logrus.Errorln("error when find policy", err)
			return nil, err
		}
	} else {
		// get policy and update policy
		newpolicy, err := c.api.GetPolicy(c.vLBSC, c.extraInfo.ProjectID, lbID, listenerID, pol.UUID)
		if err != nil {
			logrus.Fatal("error when get policy", err)
			return nil, err
		}
		comparePolicy := func(p2 *lObjects.Policy) bool {
			if string(policyOpt.Action) != p2.Action ||
				policyOpt.RedirectPoolID != p2.RedirectPoolID ||
				policyOpt.Name != p2.Name {
				return false
			}
			if len(policyOpt.Rules) != len(p2.L7Rules) {
				return false
			}

			checkIfExist := func(rules []*lObjects.L7Rule, rule policy.Rule) bool {
				for _, r := range rules {
					if r.CompareType == string(rule.CompareType) &&
						r.RuleType == string(rule.RuleType) &&
						r.RuleValue == rule.RuleValue {
						return true
					}
				}
				return false
			}
			for _, rule := range policyOpt.Rules {
				if !checkIfExist(p2.L7Rules, rule) {
					logrus.Infof("rule not exist: %v", rule)
					return false
				}
			}
			return true
		}
		if !comparePolicy(newpolicy) {
			updateOpts := &policy.UpdateOptsBuilder{
				Action:         policyOpt.Action,
				RedirectPoolID: policyOpt.RedirectPoolID,
				Rules:          policyOpt.Rules,
			}
			err := c.api.UpdatePolicy(c.vLBSC, c.extraInfo.ProjectID, lbID, listenerID, pol.UUID, updateOpts)
			if err != nil {
				logrus.Fatal("error when update policy", err)
				return nil, err
			}
		}
	}
	c.WaitForLBActive(lbID)
	pol, err = c.api.GetPolicy(c.vLBSC, c.extraInfo.ProjectID, lbID, listenerID, pol.UUID)
	if err != nil {
		logrus.Fatal("error when get policy", err)
		return nil, err
	}
	return pol, nil
}

// API
func (c *VLBProvider) ListLoadBalancerBySubnetID() {
	klog.Infof("--------------- ListLoadBalancerBySubnetID -------------------")
	c.lbsInSubnet, _ = c.api.ListLBBySubnetID(c.vLBSC, c.extraInfo.ProjectID, c.cluster.SubnetID)
	for _, lb := range c.lbsInSubnet {
		klog.Infof("lb: %v", lb)
	}
}

func (c *VLBProvider) WaitForLBActive(lbID string) *lObjects.LoadBalancer {
	for {
		lb, err := c.api.GetLB(c.vLBSC, lbID, c.extraInfo.ProjectID)
		if err != nil {
			logrus.Errorln("error when get lb status: ", err)
		} else if lb.Status == "ACTIVE" {
			return lb
		}
		logrus.Infoln("------- wait for lb active:", lb.Status, "-------")
		time.Sleep(5 * time.Second)
	}
}

func (c *VLBProvider) FindPoolByName(lbID, name string) (*lObjects.Pool, error) {
	pools, err := c.api.ListPoolOfLB(c.vLBSC, lbID, c.extraInfo.ProjectID)
	if err != nil {
		return nil, err
	}
	for _, pool := range pools {
		if pool.Name == name {
			return pool, nil
		}
	}
	return nil, errors.ErrNotFound
}

func (c *VLBProvider) FindListenerByName(lbID, name string) (*lObjects.Listener, error) {
	listeners, err := c.api.ListListenerOfLB(c.vLBSC, lbID, c.extraInfo.ProjectID)
	if err != nil {
		return nil, err
	}
	for _, listener := range listeners {
		if listener.Name == name {
			return listener, nil
		}
	}
	return nil, errors.ErrNotFound
}

func EncodeToValidName(str string) string {
	// Only letters (a-z, A-Z, 0-9, '_', '.', '-') are allowed.
	// the other char will repaced by ":{number}:"
	for _, char := range str {
		if char >= 'a' && char <= 'z' {
			continue
		}
		if char >= 'A' && char <= 'Z' {
			continue
		}
		if char >= '0' && char <= '9' {
			continue
		}
		if char == '_' || char == '.' || char == '-' {
			continue
		}
		str = strings.ReplaceAll(str, string(char), fmt.Sprintf("-%d-", char))
	}
	return str
}
func DecodeFromValidName(str string) string {
	r, _ := regexp.Compile("-[0-9]+-")
	matchs := r.FindStringSubmatch(str)
	for _, match := range matchs {
		number, _ := strconv.Atoi(match[1 : len(match)-1])
		str = strings.ReplaceAll(str, match, fmt.Sprintf("%c", number))
	}
	return str
}

// hash a string to a string have 10 char
func HashString(str string) string {
	// Create a new SHA-256 hash
	hasher := sha256.New()
	// Write the input string to the hash
	hasher.Write([]byte(str))
	// Sum returns the hash as a byte slice
	hashBytes := hasher.Sum(nil)
	// Truncate the hash to 10 characters
	truncatedHash := hashBytes[:10]
	// Convert the truncated hash to a hex-encoded string
	hashString := hex.EncodeToString(truncatedHash)
	return hashString
}
