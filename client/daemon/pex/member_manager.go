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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	dfdaemonv1 "d7y.io/api/v2/pkg/apis/dfdaemon/v1"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"d7y.io/dragonfly/v2/pkg/dfnet"
	"d7y.io/dragonfly/v2/pkg/net/ip"
	dfdaemonclient "d7y.io/dragonfly/v2/pkg/rpc/dfdaemon/client"
)

const (
	GRPCMetadataHostID = "HostID"
)

type peerExchangeMemberManager struct {
	logger *logger.SugaredLoggerOnWith

	GRPCCredentials credentials.TransportCredentials
	GRPCDialTimeout time.Duration
	GRPCDialOptions []grpc.DialOption

	peerUpdateChan chan *dfdaemonv1.PeerExchangeData

	nodes      sync.Map
	peerPool   *peerPool
	memberPool *memberPool

	localMember *MemberMeta
}

func newPeerExchangeMemberManager(localMember *MemberMeta) *peerExchangeMemberManager {
	pp := newPeerPool()
	mp := newMemberPool(pp)
	manager := &peerExchangeMemberManager{
		logger:          logger.With("component", "peerExchangeCluster"),
		GRPCCredentials: insecure.NewCredentials(), // TODO
		GRPCDialTimeout: time.Minute,               // TODO
		peerUpdateChan:  make(chan *dfdaemonv1.PeerExchangeData, 1000),
		nodes:           sync.Map{},
		peerPool:        pp,
		memberPool:      mp,
		localMember:     localMember,
	}
	go manager.broadcastInBackground()
	return manager
}

func (p *peerExchangeMemberManager) isLocal(meta *MemberMeta) bool {
	return p.localMember.HostID == meta.HostID
}

func (p *peerExchangeMemberManager) NotifyJoin(node *memberlist.Node) {
	member, err := ExtractNodeMeta(node)
	if err != nil {
		p.logger.Errorf("failed to extract node meta %s(%#v): %s", string(node.Meta), node, err)
		return
	}
	p.logger.Infof("member %s joined, ip: %s, rpc port: %d, proxy port: %d",
		member.HostID, member.IP, member.RpcPort, member.ProxyPort)
	p.syncNode(member)
}

func (p *peerExchangeMemberManager) NotifyLeave(node *memberlist.Node) {
	member, err := ExtractNodeMeta(node)
	if err != nil {
		p.logger.Errorf("failed to extract node meta %s(%#v): %s", string(node.Meta), node, err)
		return
	}
	p.logger.Infof("member %s leaved", member.HostID)
	p.memberPool.UnRegister(member.HostID)
}

func (p *peerExchangeMemberManager) NotifyUpdate(node *memberlist.Node) {
	addr := node.Addr.String()
	p.logger.Infof("member %s updated", addr)
}

func ExtractNodeMeta(node *memberlist.Node) (*MemberMeta, error) {
	nodeMeta := &MemberMeta{}
	err := json.Unmarshal(node.Meta, nodeMeta)
	if err != nil {
		return nil, err
	}

	if nodeMeta.IP == "" {
		nodeMeta.IP = node.Addr.String()
	}
	return nodeMeta, nil
}

func (p *peerExchangeMemberManager) syncNode(member *MemberMeta) {
	if p.isLocal(member) {
		p.logger.Debugf("skip local node: %s", member.IP)
		return
	}

	// random backoff for bidirectional stream
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	time.Sleep(time.Duration(r.Intn(100)) * time.Millisecond)

	if p.memberPool.IsRegistered(member.HostID) {
		p.logger.Debugf("node %s is already registered", member.HostID)
		return
	}

	grpcClient, peerExchangeClient, err := p.connectMember(member)
	if err != nil {
		p.logger.Errorf("failed to dial %s: %s", member.IP, err)
		return
	}

	closeFunc := func() error {
		_ = peerExchangeClient.CloseSend()
		return grpcClient.Close()
	}

	err = p.memberPool.Register(member.HostID, NewPeerMetadataSendReceiveCloser(peerExchangeClient, closeFunc))
	if errors.Is(err, ErrIsAlreadyExists) {
		p.logger.Debugf("node %s is already registered", member.HostID)
		return
	}

	p.logger.Infof("connected to %s, %s start receive peer metadata", member.HostID, p.localMember.HostID)

	go func() {
		defer p.memberPool.UnRegister(member.HostID)
		var data *dfdaemonv1.PeerExchangeData
		for {
			data, err = peerExchangeClient.Recv()
			if err != nil {
				p.logger.Errorf("failed to receive peer metadata: %s, member: %s", err, member.HostID)
				return
			}
			p.peerPool.Sync(member, data)
		}
	}()
}

func (p *peerExchangeMemberManager) connectMember(meta *MemberMeta) (dfdaemonclient.V1, dfdaemonv1.Daemon_PeerExchangeClient, error) {
	formatIP, ok := ip.FormatIP(meta.IP)
	if !ok {
		return nil, nil, fmt.Errorf("failed to format ip: %s", meta.IP)
	}

	netAddr := &dfnet.NetAddr{
		Type: dfnet.TCP,
		Addr: fmt.Sprintf("%s:%d", formatIP, meta.RpcPort),
	}

	credentialOpt := grpc.WithTransportCredentials(p.GRPCCredentials)
	dialOptions := append(p.GRPCDialOptions, credentialOpt, grpc.WithBlock())
	dialCtx, cancel := context.WithTimeout(context.Background(), p.GRPCDialTimeout)
	grpcClient, err := dfdaemonclient.GetV1(dialCtx, netAddr.String(), dialOptions...)
	cancel()

	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial grpc %s: %s", netAddr.String(), err)
	}

	ctx := metadata.AppendToOutgoingContext(context.Background(), GRPCMetadataHostID, p.localMember.HostID)
	peerExchangeClient, err := grpcClient.PeerExchange(ctx)
	if err != nil {
		_ = grpcClient.Close()
		return nil, nil, fmt.Errorf("failed to call %s PeerExchange: %s", netAddr.String(), err)
	}

	return grpcClient, peerExchangeClient, nil
}

func (p *peerExchangeMemberManager) broadcast(data *dfdaemonv1.PeerExchangeData) {
	p.peerUpdateChan <- data
}

func (p *peerExchangeMemberManager) broadcastInBackground() {
	for data := range p.peerUpdateChan {
		p.memberPool.broadcast(data)
	}
}
