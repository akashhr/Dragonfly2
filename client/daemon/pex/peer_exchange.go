/*
 *     Copyright 2023 The Dragonfly Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package pex

import (
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/pkg/errors"

	dfdaemonv1 "d7y.io/api/v2/pkg/apis/dfdaemon/v1"
	schedulerv1 "d7y.io/api/v2/pkg/apis/scheduler/v1"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"d7y.io/dragonfly/v2/pkg/net/ip"
)

type peerExchange struct {
	config        *peerExchangeConfig
	localMember   *memberlist.Node
	memberlist    *memberlist.Memberlist
	memberManager *peerExchangeMemberManager
	lister        InitialMemberLister
	stopCh        chan struct{}
}

type peerExchangeConfig struct {
	initialRetryInterval time.Duration
}

func WithAdvertiseAddr(advertiseAddr string) func(*memberlist.Config, *peerExchangeConfig) {
	return func(memberConfig *memberlist.Config, pexConfig *peerExchangeConfig) {
		memberConfig.AdvertiseAddr = advertiseAddr
	}
}

func WithAdvertisePort(advertisePort int) func(*memberlist.Config, *peerExchangeConfig) {
	return func(memberConfig *memberlist.Config, pexConfig *peerExchangeConfig) {
		memberConfig.AdvertisePort = advertisePort
	}
}

func WithInitialRetryInterval(initialRetryInterval time.Duration) func(*memberlist.Config, *peerExchangeConfig) {
	return func(memberConfig *memberlist.Config, pexConfig *peerExchangeConfig) {
		pexConfig.initialRetryInterval = initialRetryInterval
	}
}

func NewPeerExchange(node *MemberMeta, lister InitialMemberLister, peerUpdateChan <-chan *dfdaemonv1.PeerMetadata, opts ...func(*memberlist.Config, *peerExchangeConfig)) (PeerExchanger, error) {
	memberManager := newPeerExchangeMemberManager(peerUpdateChan)

	memberConfig := memberlist.DefaultLANConfig()
	memberConfig.Delegate = newPeerExchangeDelegate(node)
	memberConfig.Events = memberManager

	pexConfig := &peerExchangeConfig{
		initialRetryInterval: time.Minute,
	}

	for _, opt := range opts {
		opt(memberConfig, pexConfig)
	}

	if memberConfig.AdvertiseAddr == "" {
		// TODO support ipv6
		memberConfig.AdvertiseAddr = ip.IPv4.String()
	}

	memberManager.localAdvertiseAddr = memberConfig.AdvertiseAddr
	memberManager.localAdvertisePort = memberConfig.AdvertisePort

	member, err := memberlist.Create(memberConfig)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to create memberlist")
	}

	pex := &peerExchange{
		config:        pexConfig,
		localMember:   member.LocalNode(),
		memberlist:    member,
		memberManager: memberManager,
		lister:        lister,
		stopCh:        make(chan struct{}),
	}
	return pex, nil
}

func (p *peerExchange) PeerExchangeMember() PeerExchangeMember {
	return p.memberManager.memberPool
}

func (p *peerExchange) PeerExchangeSynchronizer() PeerExchangeSynchronizer {
	return p.memberManager.peerPool
}

func (p *peerExchange) PeerSearcher() PeerSearcher {
	return p
}

func (p *peerExchange) FindPeersByTask(task string) ([]*schedulerv1.PeerPacket_DestPeer, bool) {
	return p.memberManager.peerPool.Find(task)
}

func (p *peerExchange) Serve() error {
	go p.reSyncMember()

	for {
		if err := p.serve(); err == nil {
			break
		}
	}

	<-p.stopCh
	return nil
}

func (p *peerExchange) serve() error {
	var (
		members []*InitialMember
		err     error
	)

	for {
		members, err = p.lister.List()
		if err == nil {
			break
		}
		logger.Errorf("failed to list initial member: %s", err)
		time.Sleep(p.config.initialRetryInterval)
	}

	var ips []string
	for _, member := range members {
		ips = append(ips, member.Ip)
	}

	_, err = p.memberlist.Join(ips)
	if err != nil {
		logger.Errorf("failed to join cluster: %s, error: %s", ips, err)
	}

	return err
}

func (p *peerExchange) Stop() error {
	close(p.stopCh)
	return p.memberlist.Leave(10 * time.Second)
}

func (p *peerExchange) reSyncMember() {
	for {
		time.Sleep(10 * time.Minute)
		members := p.memberlist.Members()
		memberAddrs := p.memberManager.memberPool.MemberKeys()
		add, del := diffMembers(members, memberAddrs)
		for _, member := range add {
			go p.memberManager.syncNode(member)
		}
		for _, ip := range del {
			p.memberManager.memberPool.UnRegister(ip)
		}
	}
}

func diffMembers(nodes []*memberlist.Node, ips []string) (add []*memberlist.Node, del []string) {
	for _, node := range nodes {
		nodeIP := node.Addr.String()
		toAdd := true

		for _, ip := range ips {
			if nodeIP == ip {
				toAdd = false
			}
		}
		if toAdd && node.State == memberlist.StateAlive {
			add = append(add, node)
		}
	}

	for _, ip := range ips {
		toDel := true
		for _, node := range nodes {
			if node.Addr.String() == ip && node.State == memberlist.StateAlive {
				toDel = false
			}
		}
		if toDel {
			del = append(del, ip)
		}
	}
	return
}
