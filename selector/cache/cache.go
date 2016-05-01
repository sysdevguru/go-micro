package cache

import (
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/micro/go-micro/registry"
	"github.com/micro/go-micro/selector"
)

type cacheSelector struct {
	so selector.Options

	// registry cache
	sync.Mutex
	cache map[string][]*registry.Service

	// used to close or reload watcher
	reload chan bool
	exit   chan bool
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

func (c *cacheSelector) quit() bool {
	select {
	case <-c.exit:
		return true
	default:
		return false
	}
}

// cp copies a service. Because we're caching handing back pointers would
// create a race condition, so we do this instead
// its fast enough
func (c *cacheSelector) cp(current []*registry.Service) []*registry.Service {
	var services []*registry.Service

	for _, service := range current {
		// copy service
		s := new(registry.Service)
		*s = *service

		// copy nodes
		var nodes []*registry.Node
		for _, node := range service.Nodes {
			n := new(registry.Node)
			*n = *node
			nodes = append(nodes, n)
		}
		s.Nodes = nodes

		// copy endpoints
		var eps []*registry.Endpoint
		for _, ep := range service.Endpoints {
			e := new(registry.Endpoint)
			*e = *ep
			eps = append(eps, e)
		}
		s.Endpoints = eps

		// append service
		services = append(services, s)
	}

	return services
}

func (c *cacheSelector) get(service string) ([]*registry.Service, error) {
	c.Lock()
	defer c.Unlock()

	// check the cache first
	services, ok := c.cache[service]

	// got results, copy and return
	if ok && len(services) > 0 {
		return c.cp(services), nil
	}

	// now ask the registry
	services, err := c.so.Registry.GetService(service)
	if err != nil {
		return nil, err

	}

	// we didn't have any results so cache
	c.cache[service] = c.cp(services)

	return services, nil
}

func (c *cacheSelector) update(res *registry.Result) {
	if res == nil || res.Service == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	services, ok := c.cache[res.Service.Name]
	if !ok {
		// we're not going to cache anything
		// unless there was already a lookup
		return
	}

	if len(res.Service.Nodes) == 0 {
		switch res.Action {
		case "delete":
			delete(c.cache, res.Service.Name)
		}
		return
	}

	// existing service found
	var service *registry.Service
	var index int
	for i, s := range services {
		if s.Version == res.Service.Version {
			service = s
			index = i
		}
	}

	switch res.Action {
	case "create", "update":
		if service == nil {
			services = append(services, res.Service)
			c.cache[res.Service.Name] = services
			return
		}

		// append old nodes to new service
		for _, cur := range service.Nodes {
			var seen bool
			for _, node := range res.Service.Nodes {
				if cur.Id == node.Id {
					seen = true
					break
				}
			}
			if !seen {
				res.Service.Nodes = append(res.Service.Nodes, cur)
			}
		}

		services[index] = res.Service
		c.cache[res.Service.Name] = services
	case "delete":
		if service == nil {
			return
		}

		var nodes []*registry.Node

		// filter cur nodes to remove the dead one
		for _, cur := range service.Nodes {
			var seen bool
			for _, del := range res.Service.Nodes {
				if del.Id == cur.Id {
					seen = true
					break
				}
			}
			if !seen {
				nodes = append(nodes, cur)
			}
		}

		if len(nodes) == 0 {
			if len(services) == 1 {
				delete(c.cache, service.Name)
			} else {
				var srvs []*registry.Service
				for _, s := range services {
					if s.Version != service.Version {
						srvs = append(srvs, s)
					}
				}
				c.cache[service.Name] = srvs
			}
			return
		}

		service.Nodes = nodes
		services[index] = service
		c.cache[res.Service.Name] = services
	}
}

// run starts the cache watcher loop
// it creates a new watcher if there's a problem
// reloads the watcher if Init is called
// and returns when Close is called
func (c *cacheSelector) run() {
	for {
		// exit early if already dead
		if c.quit() {
			return
		}

		// create new watcher
		w, err := c.so.Registry.Watch()
		if err != nil {
			log.Println(err)
			time.Sleep(time.Second)
			continue
		}

		// manage this loop
		go func() {
			// wait for exit or reload signal
			select {
			case <-c.exit:
			case <-c.reload:
			}

			// stop the watcher
			w.Stop()
		}()

		// watch for events
		if err := c.watch(w); err != nil {
			log.Println(err)
			continue
		}
	}
}

// watch loops the next event and calls update
// it returns if there's an error
func (c *cacheSelector) watch(w registry.Watcher) error {
	for {
		res, err := w.Next()
		if err != nil {
			return err
		}
		c.update(res)
	}
}

func (c *cacheSelector) Init(opts ...selector.Option) error {
	for _, o := range opts {
		o(&c.so)
	}

	// reload the watcher
	go func() {
		select {
		case <-c.exit:
			return
		default:
			c.reload <- true
		}
	}()

	return nil
}

func (c *cacheSelector) Options() selector.Options {
	return c.so
}

func (c *cacheSelector) Select(service string, opts ...selector.SelectOption) (selector.Next, error) {
	var sopts selector.SelectOptions
	for _, opt := range opts {
		opt(&sopts)
	}

	// get the service
	// try the cache first
	// if that fails go directly to the registry
	services, err := c.get(service)
	if err != nil {
		return nil, err
	}

	// apply the filters
	for _, filter := range sopts.Filters {
		services = filter(services)
	}

	// if there's nothing left, return
	if len(services) == 0 {
		return nil, selector.ErrNotFound
	}

	var nodes []*registry.Node

	for _, service := range services {
		for _, node := range service.Nodes {
			nodes = append(nodes, node)
		}
	}

	if len(nodes) == 0 {
		return nil, selector.ErrNotFound
	}

	return func() (*registry.Node, error) {
		i := rand.Int()
		j := i % len(services)

		if len(services[j].Nodes) == 0 {
			return nil, selector.ErrNotFound
		}

		k := i % len(services[j].Nodes)
		return services[j].Nodes[k], nil
	}, nil
}

func (c *cacheSelector) Mark(service string, node *registry.Node, err error) {
	return
}

func (c *cacheSelector) Reset(service string) {
	c.Lock()
	delete(c.cache, service)
	c.Unlock()
}

// Close stops the watcher and destroys the cache
func (c *cacheSelector) Close() error {
	c.Lock()
	c.cache = make(map[string][]*registry.Service)
	c.Unlock()

	select {
	case <-c.exit:
		return nil
	default:
		close(c.exit)
	}
	return nil
}

func (c *cacheSelector) String() string {
	return "cache"
}

func NewSelector(opts ...selector.Option) selector.Selector {
	var sopts selector.Options

	for _, opt := range opts {
		opt(&sopts)
	}

	if sopts.Registry == nil {
		sopts.Registry = registry.DefaultRegistry
	}

	c := &cacheSelector{
		so:     sopts,
		cache:  make(map[string][]*registry.Service),
		reload: make(chan bool, 1),
		exit:   make(chan bool),
	}

	go c.run()

	return c
}