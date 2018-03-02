package plugin

import (
	"os"

	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/terraform/terraform"
)

// The constants below are the names of the plugins that can be dispensed
// from the plugin server.
const (
	ProviderPluginName    = "provider"
	ProvisionerPluginName = "provisioner"
)

const (
	// TerraformPLuginProtocol is an environment variable used to indicate the
	// protocol being requested by the client. The only valid value for this is
	// TerraformProtoGRPC
	TerraformPluginProtocol = "TERRAFORM_PLUGIN_PROTOCOL"
	TerraformProtoGRPC      = "grpc"
)

// Handshake is the HandshakeConfig used to configure clients and servers.
var Handshake = plugin.HandshakeConfig{
	// The ProtocolVersion is the version that must match between TF core
	// and TF plugins. This should be bumped whenever a change happens in
	// one or the other that makes it so that they can't safely communicate.
	// This could be adding a new interface value, it could be how
	// helper/schema computes diffs, etc.
	ProtocolVersion: 4,

	// The magic cookie values should NEVER be changed.
	MagicCookieKey:   "TF_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2",
}

type ProviderFunc func() terraform.ResourceProvider
type ProvisionerFunc func() terraform.ResourceProvisioner

// ServeOpts are the configurations to serve a plugin.
type ServeOpts struct {
	ProviderFunc    ProviderFunc
	ProvisionerFunc ProvisionerFunc
}

// Serve serves a plugin. This function never returns and should be the final
// function called in the main function of the plugin.
func Serve(opts *ServeOpts) {
	switch os.Getenv(TerraformPluginProtocol) {
	case TerraformProtoGRPC:
		plugin.Serve(&plugin.ServeConfig{
			HandshakeConfig: Handshake,
			Plugins:         pluginMap(opts),
			GRPCServer:      plugin.DefaultGRPCServer,
		})
	default:
		panic("protocol not supported")
	}

}

// pluginMap returns the map[string]plugin.Plugin to use for configuring a plugin
// server or client.
func pluginMap(opts *ServeOpts) map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		"provider":    &ResourceProviderPlugin{F: opts.ProviderFunc},
		"provisioner": &ResourceProvisionerPlugin{F: opts.ProvisionerFunc},
	}
}
