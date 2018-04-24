package terraform

import (
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/hcl2/hcl"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/dag"
)

func TransformProviders(providers []string, concrete ConcreteProviderNodeFunc, config *configs.Config) GraphTransformer {
	return GraphTransformMulti(
		// Add providers from the config
		&ProviderConfigTransformer{
			Config:    config,
			Providers: providers,
			Concrete:  concrete,
		},
		// Add any remaining missing providers
		&MissingProviderTransformer{
			Providers: providers,
			Concrete:  concrete,
		},
		// Connect the providers
		&ProviderTransformer{},
		// Remove unused providers and proxies
		&PruneProviderTransformer{},
		// Connect provider to their parent provider nodes
		&ParentProviderTransformer{},
	)
}

// GraphNodeProvider is an interface that nodes that can be a provider
// must implement.
//
// ProviderAddr returns the address of the provider configuration this
// satisfies, which is relative to the path returned by method Path().
//
// Name returns the full name of the provider in the config.
type GraphNodeProvider interface {
	GraphNodeSubPath
	ProviderAddr() addrs.AbsProviderConfig
	Name() string
}

// GraphNodeCloseProvider is an interface that nodes that can be a close
// provider must implement. The CloseProviderName returned is the name of
// the provider they satisfy.
type GraphNodeCloseProvider interface {
	GraphNodeSubPath
	CloseProviderAddr() addrs.AbsProviderConfig
}

// GraphNodeProviderConsumer is an interface that nodes that require
// a provider must implement. ProvidedBy must return the address of the provider
// to use, which will be resolved to a configuration either in the same module
// or in an ancestor module, with the resulting absolute address passed to
// SetProvider.
type GraphNodeProviderConsumer interface {
	// ProvidedBy returns the address of the provider configuration the node
	// refers to. If the returned "exact" value is true, this address will
	// be taken exactly. If "exact" is false, a provider configuration from
	// an ancestor module may be selected instead.
	ProvidedBy() (addr addrs.AbsProviderConfig, exact bool)
	// Set the resolved provider address for this resource.
	SetProvider(addrs.AbsProviderConfig)
}

// ProviderTransformer is a GraphTransformer that maps resources to
// providers within the graph. This will error if there are any resources
// that don't map to proper resources.
type ProviderTransformer struct{}

func (t *ProviderTransformer) Transform(g *Graph) error {
	// Go through the other nodes and match them to providers they need
	var err error
	m := providerVertexMap(g)
	for _, v := range g.Vertices() {
		if pv, ok := v.(GraphNodeProviderConsumer); ok {
			p, exact := pv.ProvidedBy()

			key := p.String()
			target := m[key]

			sp, ok := pv.(GraphNodeSubPath)
			if !ok && target == nil {
				// no target, and no path to walk up
				err = multierror.Append(err, fmt.Errorf(
					"%s: provider %s couldn't be found",
					dag.VertexName(v), p))
				break
			}

			// if we don't have a provider at this level, walk up the path looking for one,
			// unless we were told to be exact.
			if !exact {
				for pp, ok := p.Inherited(); ok; pp, ok = pp.Inherited() {
					key := pp.String()
					target = m[key]
					if target != nil {
						break
					}
				}
			}

			if target == nil {
				err = multierror.Append(err, fmt.Errorf(
					"%s: configuration for %s is not present; a provider configuration block is required for all operations",
					dag.VertexName(v), p,
				))
				break
			}

			// see if this in  an inherited provider
			if p, ok := target.(*graphNodeProxyProvider); ok {
				g.Remove(p)
				target = p.Target()
				key = target.(GraphNodeProvider).ProviderAddr().String()
			}

			log.Printf("[DEBUG] resource %s using provider %s", dag.VertexName(pv), key)
			pv.SetProvider(target.ProviderAddr())
			g.Connect(dag.BasicEdge(v, target))
		}
	}

	return err
}

// CloseProviderTransformer is a GraphTransformer that adds nodes to the
// graph that will close open provider connections that aren't needed anymore.
// A provider connection is not needed anymore once all depended resources
// in the graph are evaluated.
type CloseProviderTransformer struct{}

func (t *CloseProviderTransformer) Transform(g *Graph) error {
	pm := providerVertexMap(g)
	cpm := make(map[string]*graphNodeCloseProvider)
	var err error

	for _, v := range pm {
		p := v.(GraphNodeProvider)
		key := p.ProviderAddr().String()

		// get the close provider of this type if we alread created it
		closer := cpm[key]

		if closer == nil {
			// create a closer for this provider type
			closer = &graphNodeCloseProvider{Addr: p.ProviderAddr()}
			g.Add(closer)
			cpm[key] = closer
		}

		// Close node depends on the provider itself
		// this is added unconditionally, so it will connect to all instances
		// of the provider. Extra edges will be removed by transitive
		// reduction.
		g.Connect(dag.BasicEdge(closer, p))

		// connect all the provider's resources to the close node
		for _, s := range g.UpEdges(p).List() {
			if _, ok := s.(GraphNodeProviderConsumer); ok {
				g.Connect(dag.BasicEdge(closer, s))
			}
		}
	}

	return err
}

// MissingProviderTransformer is a GraphTransformer that adds nodes for all
// required providers into the graph. Specifically, it creates provider
// configuration nodes for all the providers that we support. These are pruned
// later during an optimization pass.
type MissingProviderTransformer struct {
	// Providers is the list of providers we support.
	Providers []string

	// Concrete, if set, overrides how the providers are made.
	Concrete ConcreteProviderNodeFunc
}

func (t *MissingProviderTransformer) Transform(g *Graph) error {
	// Initialize factory
	if t.Concrete == nil {
		t.Concrete = func(a *NodeAbstractProvider) dag.Vertex {
			return a
		}
	}

	var err error
	m := providerVertexMap(g)
	for _, v := range g.Vertices() {
		pv, ok := v.(GraphNodeProviderConsumer)
		if !ok {
			continue
		}

		p, _ := pv.ProvidedBy()
		configAddr := p.ProviderConfig
		key := configAddr.String()
		provider := m[key]

		// we already have it
		if provider != nil {
			continue
		}

		// we don't implicitly create aliased providers
		if configAddr.Alias != "" {
			log.Println("[DEBUG] not adding implicit aliased config for ", p.String())
			continue
		}

		log.Println("[DEBUG] adding implicit configuration for ", p.String())

		// create the missing top-level provider
		provider = t.Concrete(&NodeAbstractProvider{
			Addr: p,
		}).(GraphNodeProvider)

		g.Add(provider)
		m[key] = provider
	}

	return err
}

// ParentProviderTransformer connects provider nodes to their parents.
//
// This works by finding nodes that are both GraphNodeProviders and
// GraphNodeSubPath. It then connects the providers to their parent
// path. The parent provider is always at the root level.
type ParentProviderTransformer struct{}

func (t *ParentProviderTransformer) Transform(g *Graph) error {
	pm := providerVertexMap(g)
	for _, v := range g.Vertices() {
		// Only care about providers
		pn, ok := v.(GraphNodeProvider)
		if !ok {
			continue
		}

		// Also require non-empty path, since otherwise we're in the root
		// module and so cannot have a parent.
		if len(pn.Path()) <= 1 {
			continue
		}

		// this provider may be disabled, but we can only get it's name from
		// the ProviderName string
		addr := pn.ProviderAddr()
		parentAddr, ok := addr.Inherited()
		if ok {
			parent := pm[parentAddr.String()]
			if parent != nil {
				g.Connect(dag.BasicEdge(v, parent))
			}
		}
	}
	return nil
}

// PruneProviderTransformer removes any providers that are not actually used by
// anything, and provider proxies. This avoids the provider being initialized
// and configured.  This both saves resources but also avoids errors since
// configuration may imply initialization which may require auth.
type PruneProviderTransformer struct{}

func (t *PruneProviderTransformer) Transform(g *Graph) error {
	for _, v := range g.Vertices() {
		// We only care about providers
		pn, ok := v.(GraphNodeProvider)
		if !ok {
			continue
		}

		// ProxyProviders will have up edges, but we're now done with them in the graph
		if _, ok := v.(*graphNodeProxyProvider); ok {
			log.Printf("[DEBUG] pruning proxy provider %s", dag.VertexName(v))
			g.Remove(v)
		}

		// Remove providers with no dependencies.
		if g.UpEdges(v).Len() == 0 {
			log.Printf("[DEBUG] pruning unused provider %s", dag.VertexName(v))
			g.Remove(v)
		}
	}

	return nil
}

func providerVertexMap(g *Graph) map[string]GraphNodeProvider {
	m := make(map[string]GraphNodeProvider)
	for _, v := range g.Vertices() {
		if pv, ok := v.(GraphNodeProvider); ok {
			addr := pv.ProviderAddr()
			m[addr.String()] = pv
		}
	}

	return m
}

func closeProviderVertexMap(g *Graph) map[string]GraphNodeCloseProvider {
	m := make(map[string]GraphNodeCloseProvider)
	for _, v := range g.Vertices() {
		if pv, ok := v.(GraphNodeCloseProvider); ok {
			addr := pv.CloseProviderAddr()
			m[addr.String()] = pv
		}
	}

	return m
}

type graphNodeCloseProvider struct {
	Addr addrs.AbsProviderConfig
}

var (
	_ GraphNodeCloseProvider = (*graphNodeCloseProvider)(nil)
)

func (n *graphNodeCloseProvider) Name() string {
	return n.Addr.String() + " (close)"
}

// GraphNodeSubPath impl.
func (n *graphNodeCloseProvider) Path() addrs.ModuleInstance {
	return n.Addr.Module
}

// GraphNodeEvalable impl.
func (n *graphNodeCloseProvider) EvalTree() EvalNode {
	return CloseProviderEvalTree(n.Addr)
}

// GraphNodeDependable impl.
func (n *graphNodeCloseProvider) DependableName() []string {
	return []string{n.Name()}
}

func (n *graphNodeCloseProvider) CloseProviderAddr() addrs.AbsProviderConfig {
	return n.Addr
}

// GraphNodeDotter impl.
func (n *graphNodeCloseProvider) DotNode(name string, opts *dag.DotOpts) *dag.DotNode {
	if !opts.Verbose {
		return nil
	}
	return &dag.DotNode{
		Name: name,
		Attrs: map[string]string{
			"label": n.Name(),
			"shape": "diamond",
		},
	}
}

// RemovableIfNotTargeted
func (n *graphNodeCloseProvider) RemoveIfNotTargeted() bool {
	// We need to add this so that this node will be removed if
	// it isn't targeted or a dependency of a target.
	return true
}

// graphNodeProxyProvider is a GraphNodeProvider implementation that is used to
// store the name and value of a provider node for inheritance between modules.
// These nodes are only used to store the data while loading the provider
// configurations, and are removed after all the resources have been connected
// to their providers.
type graphNodeProxyProvider struct {
	addr addrs.AbsProviderConfig
	nameValue string
	path      []string
	target    GraphNodeProvider
}

func (n *graphNodeProxyProvider) ProviderAddr() addrs.AbsProviderConfig {
	return n.addr
}

func (n *graphNodeProxyProvider) Path() addrs.ModuleInstance {
	return n.addr.Module
}

func (n *graphNodeProxyProvider) Name() string {
	return n.addr.String()
}

// find the concrete provider instance
func (n *graphNodeProxyProvider) Target() GraphNodeProvider {
	switch t := n.target.(type) {
	case *graphNodeProxyProvider:
		return t.Target()
	default:
		return n.target
	}
}

// ProviderConfigTransformer adds all provider nodes from the configuration and
// attaches the configs.
type ProviderConfigTransformer struct {
	Providers []string
	Concrete  ConcreteProviderNodeFunc

	// each provider node is stored here so that the proxy nodes can look up
	// their targets by name.
	providers map[string]GraphNodeProvider
	// record providers that can be overriden with a proxy
	proxiable map[string]bool

	// Config is the root node of the configuration tree to add providers from.
	Config *configs.Config
}

func (t *ProviderConfigTransformer) Transform(g *Graph) error {
	// If no configuration is given, we don't do anything
	if t.Config == nil {
		return nil
	}

	t.providers = make(map[string]GraphNodeProvider)
	t.proxiable = make(map[string]bool)

	// Start the transformation process
	if err := t.transform(g, t.Config); err != nil {
		return err
	}

	// finally attach the configs to the new nodes
	return t.attachProviderConfigs(g)
}

func (t *ProviderConfigTransformer) transform(g *Graph, c *configs.Config) error {
	// If no config, do nothing
	if c == nil {
		return nil
	}

	// Add our resources
	if err := t.transformSingle(g, c); err != nil {
		return err
	}

	// Transform all the children.
	for _, cc := range c.Children {
		if err := t.transform(g, cc); err != nil {
			return err
		}
	}
	return nil
}

func (t *ProviderConfigTransformer) transformSingle(g *Graph, c *configs.Config) error {
	log.Printf("[TRACE] ProviderConfigTransformer: Starting for module %q", c.Path.String())

	// Get the module associated with this configuration tree node
	mod := c.Module
	staticPath := c.Path

	// We actually need a dynamic module path here, but we've not yet updated
	// our graph builders enough to support expansion of module calls with
	// "count" and "for_each" set, so for now we'll shim this by converting to
	// a dynamic path with no keys. At the time of writing this is the only
	// possible kind of dynamic path anyway.
	path := make(addrs.ModuleInstance, len(staticPath))
	for i, name := range staticPath {
		path[i] = addrs.ModuleInstanceStep{
			Name: name,
		}
	}

	// add all providers from the configuration
	for _, p := range mod.ProviderConfigs {
		name := p.Name
		relAddr := p.Addr()
		addr := relAddr.Absolute(path)

		v := t.Concrete(&NodeAbstractProvider{
			Addr: addr,
		})

		// Add it to the graph
		g.Add(v)
		key := addr.String()
		t.providers[key] = v.(GraphNodeProvider)

		// A provider configuration is "proxyable" if its configuration is
		// entirely empty. This means it's standing in for a provider
		// configuration that must be passed in from the parent module.
		// We decide this by evaluating the config with an empty schema;
		// if this succeeds, then we know there's nothing in the body.
		_, diags := p.Config.Content(&hcl.BodySchema{})
		t.proxiable[key] = !diags.HasErrors()
	}

	// Now replace the provider nodes with proxy nodes if a provider was being
	// passed in, and create implicit proxies if there was no config. Any extra
	// proxies will be removed in the prune step.
	return t.addProxyProviders(g, c)
}

func (t *ProviderConfigTransformer) addProxyProviders(g *Graph, c *configs.Config) error {
	path := c.Path

	// can't add proxies at the root
	if len(path) == 0 {
		return nil
	}

	parentPath, callName := path[:len(path)-1], path[len(path)-1]
	parent := c.Descendent(parentPath)
	if parent == nil {
		return nil
	}

	var parentCfg *configs.ModuleCall
	for name, mod := range parent.Module.ModuleCalls {
		if name == callName {
			parentCfg = mod
			break
		}
	}

	if parentCfg == nil {
		// this can't really happen during normal execution.
		return fmt.Errorf("parent module config not found for %s", m.Name())
	}

	// Go through all the providers the parent is passing in, and add proxies to
	// the parent provider nodes.
	for name, parentName := range parentCfg.ProviderConfigs {
		fullName := ResolveProviderName(name, path)
		fullParentName := ResolveProviderName(parentName, parentPath)

		parentProvider := t.providers[fullParentName]

		if parentProvider == nil {
			return fmt.Errorf("missing provider %s", fullParentName)
		}

		proxy := &graphNodeProxyProvider{
			nameValue: name,
			path:      path,
			target:    parentProvider,
		}

		concreteProvider := t.providers[fullName]

		// replace the concrete node with the provider passed in
		if concreteProvider != nil && t.proxiable[fullName] {
			g.Replace(concreteProvider, proxy)
			t.providers[fullName] = proxy
			continue
		}

		// aliased providers can't be implicitly passed in
		if strings.Contains(name, ".") {
			continue
		}

		// There was no concrete provider, so add this as an implicit provider.
		// The extra proxy will be pruned later if it's unused.
		g.Add(proxy)
		t.providers[fullName] = proxy
	}
	return nil
}

func (t *ProviderConfigTransformer) attachProviderConfigs(g *Graph) error {
	for _, v := range g.Vertices() {
		// Only care about GraphNodeAttachProvider implementations
		apn, ok := v.(GraphNodeAttachProvider)
		if !ok {
			continue
		}

		// Determine what we're looking for
		path := apn.Path()
		name := apn.ProviderName()
		log.Printf("[TRACE] Attach provider request: %#v %s", path, name)

		// Get the configuration.
		mc := t.Config.DescendentForInstance(path)
		if mc == nil {
			continue
		}

		// Go through the provider configs to find the matching config
		for _, p := range mc.Module.Providers {
			// Build the name, which is "name.alias" if an alias exists
			current := p.Name
			if p.Alias != "" {
				current += "." + p.Alias
			}

			// If the configs match then attach!
			if current == name {
				log.Printf("[TRACE] Attaching provider config: %#v", p)
				apn.AttachProvider(p)
				break
			}
		}
	}

	return nil
}
