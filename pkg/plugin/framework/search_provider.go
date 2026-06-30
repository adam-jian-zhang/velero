/*
Copyright The Velero Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

import (
	"context"

	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/vmware-tanzu/velero/pkg/plugin/framework/common"
	"github.com/vmware-tanzu/velero/pkg/plugin/generated"
)

type SearchProviderPlugin struct {
	hcplugin.NetRPCUnsupportedPlugin
	*common.PluginBase
}

func NewSearchProviderPlugin(options ...common.PluginOption) *SearchProviderPlugin {
	return &SearchProviderPlugin{
		PluginBase: common.NewPluginBase(options...),
	}
}

func (p *SearchProviderPlugin) GRPCClient(_ context.Context, _ *hcplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	return common.NewClientDispenser(p.ClientLogger, conn, newSearchProviderGRPCClient), nil
}

func (p *SearchProviderPlugin) GRPCServer(_ *hcplugin.GRPCBroker, server *grpc.Server) error {
	generated.RegisterSearchProviderServer(server, &SearchProviderGRPCServer{mux: p.ServerMux})
	return nil
}
