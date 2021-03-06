package dnsproxy

import (
	"errors"
	"github.com/armon/go-metrics"
	"github.com/miekg/dns"
	"github.com/prometheus/common/log"
	"sync"
	"time"
)

const (
	ByOrderLB uint8 = iota
	RoundRobinLB uint8 = iota
	AllGroupName = "all"
)

type UpstreamServer struct {
	Address     string            `mapstructure:"Address"`
	Annotations map[string]string `mapstructure:"Annotations"`
}

type ServersView []*UpstreamServer

type UpstreamsManager struct {
	Servers []UpstreamServer
	LBType  uint8

	rrLB             *IndexRoundRobin
	regionMap        *RegionMap
	serversRegionMap map[string]ServersView

	Timeout time.Duration
}

func NewServerView(size uint) ServersView {
	return make([]*UpstreamServer, size)
}

func NewUpstreamsManager(servers []UpstreamServer, lbType string, regionMap *RegionMap, timeout string) *UpstreamsManager {
	usm := new(UpstreamsManager)
	usm.serversRegionMap = make(map[string]ServersView)
	usm.Servers = servers
	var err error
	usm.Timeout, err = time.ParseDuration(timeout)
	if err != nil {
		log.Fatal("Failed to parse Timeout")
	}
	if lbType == "RoundRobin" {
		usm.LBType = RoundRobinLB
		usm.rrLB = &IndexRoundRobin{
			current: 0,
			max: len(usm.Servers),
		}
	} else {
		usm.LBType = ByOrderLB
	}
	usm.regionMap = regionMap

	for i, _:= range usm.Servers {
		srv := &(usm.Servers[i])
		if region, ok := srv.Annotations["region"]; ok {
			usm.serversRegionMap[region] = append(usm.serversRegionMap[region], srv)
		}
		// Include all Upstreams to "all" region group
		usm.serversRegionMap[AllGroupName] = append(usm.serversRegionMap[AllGroupName], srv)
	}

	return usm
}

func (usm *UpstreamsManager) Name() string {
	return "UpstreamManager"
}

func (usm *UpstreamsManager) Apply(query *EngineQuery, metadata RequestMetadata) (*EngineQuery, error) {
	result := new(EngineQuery)
	result.Queries = query.Queries
	if len(query.Queries) <= 0 {
		return nil, errors.New("can't get as input empty EngineQuery")
	}

	// Run on each query
	// First query is the original after rewrites
	// Second and later are fallback rules
	for _, q := range query.Queries {
		// Build upstream message and forward to Upstream Servers
		upsRequest := usm.buildUpstreamMsg(query.dnsMsg, q)
		resp := usm.forwardRequest(upsRequest, metadata)

		// If response is not valid continue to next fallback query
		if resp != nil {
			return &EngineQuery{
				Queries: query.Queries,
				Result:  ALLOWED,
				dnsMsg:  resp,
			}, nil
		}
	}

	return nil, errors.New("failed to get response from Upstream Servers")
}

func (usm *UpstreamsManager) buildUpstreamMsg(originReq *dns.Msg, query Query) *dns.Msg {
	upstreamMsg := new(dns.Msg)
	originReq.CopyTo(upstreamMsg)
	upstreamMsg.Question = make([]dns.Question, 1)
	upstreamMsg.Question[0] = dns.Question{
		Name:   query.Name,
		Qtype:  query.Type,
		Qclass: originReq.Question[0].Qclass,
	}
	return upstreamMsg
}

// Internal function of passing requests to the upstream DNS server
func (usm *UpstreamsManager) forwardRequest(req *dns.Msg, meta RequestMetadata) *dns.Msg {
	startTime := time.Now()
	// Create a DNS client
	client := new(dns.Client)

	// Make a request to the upstream server
	var remoteHost string
	err, servers := usm.UpstreamSelector(req, meta)
	if err != nil {
		return nil
	}

	currentTime := time.Now()
	for i :=0; currentTime.Before(startTime.Add(usm.Timeout)); i++ {
		if usm.LBType == RoundRobinLB {
			remoteHost = servers[usm.rrLB.LimitedGet(len(servers))].Address
		} else {
			remoteHost = servers[i].Address
		}
		resp, _, err := client.Exchange(req, remoteHost)
		if globalConfig.Telemetry.Enabled {
			metrics.IncrCounterWithLabels([]string{"hoopoe", "request_count"}, 1, []metrics.Label{
				{
					Name:  "remoteHost",
					Value: remoteHost,
				},
			})
		}

		if err != nil {
			if globalConfig.Telemetry.Enabled {
				metrics.IncrCounterWithLabels([]string{"hoopoe", "request_failed"}, 1, []metrics.Label{
					{
						Name:  "remoteHost",
						Value: remoteHost,
					},
				})
			}
			log.Warnf("Error while contacting server: %s, message: %s", remoteHost, err)
		} else if len(resp.Answer) > 0 {
			return resp
		}
		currentTime = time.Now()
	}

	return nil
}

// Get Matching Upstream Servers
func (usm *UpstreamsManager) UpstreamSelector(req *dns.Msg, meta RequestMetadata) (error, ServersView) {
	// Skip region checking if region map do not exists
	if usm.regionMap == nil {
		goto allServers
	}

	// Get regional upstream servers
	if serversList, ok := usm.serversRegionMap[meta.Region]; ok {
		return nil, serversList
	} else {
		// Fallback to All server group
		goto allServers
	}

	allServers:
		return nil, usm.serversRegionMap[AllGroupName]
}

type IndexRoundRobin struct {
	sync.Mutex

	current int
	max     int
}

func (r *IndexRoundRobin) Get() int {
	r.Lock()
	defer r.Unlock()

	if r.current >= r.max {
		r.current = r.current % r.max
	}

	result := r.current
	r.current++
	return result
}

func (r *IndexRoundRobin) LimitedGet(max int) int {
	r.Lock()
	defer r.Unlock()

	if r.current >= max {
		r.current = r.current % max
	}

	result := r.current
	r.current++
	return result
}
