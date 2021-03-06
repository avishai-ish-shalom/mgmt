// Mgmt
// Copyright (C) 2013-2016+ James Shubin and the project contributors
// Written by James Shubin <james@shubin.ca> and the project contributors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package gconfig provides the facilities for loading a graph from a yaml file.
package gconfig

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"reflect"
	"strings"

	"github.com/purpleidea/mgmt/etcd"
	"github.com/purpleidea/mgmt/event"
	"github.com/purpleidea/mgmt/global"
	"github.com/purpleidea/mgmt/pgraph"
	"github.com/purpleidea/mgmt/resources"
	"github.com/purpleidea/mgmt/util"

	"gopkg.in/yaml.v2"
)

type collectorResConfig struct {
	Kind    string `yaml:"kind"`
	Pattern string `yaml:"pattern"` // XXX: Not Implemented
}

type vertexConfig struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

type edgeConfig struct {
	Name string       `yaml:"name"`
	From vertexConfig `yaml:"from"`
	To   vertexConfig `yaml:"to"`
}

// GraphConfig is the data structure that describes a single graph to run.
type GraphConfig struct {
	Graph     string `yaml:"graph"`
	Resources struct {
		Noop  []*resources.NoopRes  `yaml:"noop"`
		Pkg   []*resources.PkgRes   `yaml:"pkg"`
		File  []*resources.FileRes  `yaml:"file"`
		Svc   []*resources.SvcRes   `yaml:"svc"`
		Exec  []*resources.ExecRes  `yaml:"exec"`
		Timer []*resources.TimerRes `yaml:"timer"`
		Msg   []*resources.MsgRes   `yaml:"msg"`
	} `yaml:"resources"`
	Collector []collectorResConfig `yaml:"collect"`
	Edges     []edgeConfig         `yaml:"edges"`
	Comment   string               `yaml:"comment"`
	Hostname  string               `yaml:"hostname"` // uuid for the host
	Remote    string               `yaml:"remote"`
}

// Parse parses a data stream into the graph structure.
func (c *GraphConfig) Parse(data []byte) error {
	if err := yaml.Unmarshal(data, c); err != nil {
		return err
	}
	if c.Graph == "" {
		return errors.New("Graph config: invalid `graph`")
	}
	return nil
}

// ParseConfigFromFile takes a filename and returns the graph config structure.
func ParseConfigFromFile(filename string) *GraphConfig {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Printf("Config: Error: ParseConfigFromFile: File: %v", err)
		return nil
	}

	var config GraphConfig
	if err := config.Parse(data); err != nil {
		log.Printf("Config: Error: ParseConfigFromFile: Parse: %v", err)
		return nil
	}

	return &config
}

// NewGraphFromConfig returns a new graph from existing input, such as from the
// existing graph, and a GraphConfig struct.
func (c *GraphConfig) NewGraphFromConfig(g *pgraph.Graph, embdEtcd *etcd.EmbdEtcd, noop bool) (*pgraph.Graph, error) {
	if c.Hostname == "" {
		return nil, fmt.Errorf("Config: Error: Hostname can't be empty!")
	}

	var graph *pgraph.Graph // new graph to return
	if g == nil {           // FIXME: how can we check for an empty graph?
		graph = pgraph.NewGraph("Graph") // give graph a default name
	} else {
		graph = g.Copy() // same vertices, since they're pointers!
	}

	var lookup = make(map[string]map[string]*pgraph.Vertex)

	//log.Printf("%+v", config) // debug

	// TODO: if defined (somehow)...
	graph.SetName(c.Graph) // set graph name

	var keep []*pgraph.Vertex        // list of vertex which are the same in new graph
	var resourceList []resources.Res // list of resources to export
	// use reflection to avoid duplicating code... better options welcome!
	value := reflect.Indirect(reflect.ValueOf(c.Resources))
	vtype := value.Type()
	for i := 0; i < vtype.NumField(); i++ { // number of fields in struct
		name := vtype.Field(i).Name // string of field name
		field := value.FieldByName(name)
		iface := field.Interface() // interface type of value
		slice := reflect.ValueOf(iface)
		// XXX: should we just drop these everywhere and have the kind strings be all lowercase?
		kind := util.FirstToUpper(name)
		if global.DEBUG {
			log.Printf("Config: Processing: %v...", kind)
		}
		for j := 0; j < slice.Len(); j++ { // loop through resources of same kind
			x := slice.Index(j).Interface()
			res, ok := x.(resources.Res) // convert to Res type
			if !ok {
				return nil, fmt.Errorf("Config: Error: Can't convert: %v of type: %T to Res.", x, x)
			}
			if noop {
				res.Meta().Noop = noop
			}
			if _, exists := lookup[kind]; !exists {
				lookup[kind] = make(map[string]*pgraph.Vertex)
			}
			// XXX: should we export based on a @@ prefix, or a metaparam
			// like exported => true || exported => (host pattern)||(other pattern?)
			if !strings.HasPrefix(res.GetName(), "@@") { // not exported resource
				// XXX: we don't have a way of knowing if any of the
				// metaparams are undefined, and as a result to set the
				// defaults that we want! I hate the go yaml parser!!!
				v := graph.GetVertexMatch(res)
				if v == nil { // no match found
					res.Init()
					v = pgraph.NewVertex(res)
					graph.AddVertex(v) // call standalone in case not part of an edge
				}
				lookup[kind][res.GetName()] = v // used for constructing edges
				keep = append(keep, v)          // append

			} else if !noop { // do not export any resources if noop
				// store for addition to etcd storage...
				res.SetName(res.GetName()[2:]) //slice off @@
				res.SetKind(kind)              // cheap init
				resourceList = append(resourceList, res)
			}
		}
	}
	// store in etcd
	if err := etcd.EtcdSetResources(embdEtcd, c.Hostname, resourceList); err != nil {
		return nil, fmt.Errorf("Config: Could not export resources: %v", err)
	}

	// lookup from etcd
	var hostnameFilter []string // empty to get from everyone
	kindFilter := []string{}
	for _, t := range c.Collector {
		// XXX: should we just drop these everywhere and have the kind strings be all lowercase?
		kind := util.FirstToUpper(t.Kind)
		kindFilter = append(kindFilter, kind)
	}
	// do all the graph look ups in one single step, so that if the etcd
	// database changes, we don't have a partial state of affairs...
	if len(kindFilter) > 0 { // if kindFilter is empty, don't need to do lookups!
		var err error
		resourceList, err = etcd.EtcdGetResources(embdEtcd, hostnameFilter, kindFilter)
		if err != nil {
			return nil, fmt.Errorf("Config: Could not collect resources: %v", err)
		}
	}
	for _, res := range resourceList {
		matched := false
		// see if we find a collect pattern that matches
		for _, t := range c.Collector {
			// XXX: should we just drop these everywhere and have the kind strings be all lowercase?
			kind := util.FirstToUpper(t.Kind)
			// use t.Kind and optionally t.Pattern to collect from etcd storage
			log.Printf("Collect: %v; Pattern: %v", kind, t.Pattern)

			// XXX: expand to more complex pattern matching here...
			if res.Kind() != kind {
				continue
			}

			if matched {
				// we've already matched this resource, should we match again?
				log.Printf("Config: Warning: Matching %v[%v] again!", kind, res.GetName())
			}
			matched = true

			// collect resources but add the noop metaparam
			if noop {
				res.Meta().Noop = noop
			}

			if t.Pattern != "" { // XXX: simplistic for now
				res.CollectPattern(t.Pattern) // res.Dirname = t.Pattern
			}

			log.Printf("Collect: %v[%v]: collected!", kind, res.GetName())

			// XXX: similar to other resource add code:
			if _, exists := lookup[kind]; !exists {
				lookup[kind] = make(map[string]*pgraph.Vertex)
			}
			v := graph.GetVertexMatch(res)
			if v == nil { // no match found
				res.Init() // initialize go channels or things won't work!!!
				v = pgraph.NewVertex(res)
				graph.AddVertex(v) // call standalone in case not part of an edge
			}
			lookup[kind][res.GetName()] = v // used for constructing edges
			keep = append(keep, v)          // append

			//break // let's see if another resource even matches
		}
	}

	// get rid of any vertices we shouldn't "keep" (that aren't in new graph)
	for _, v := range graph.GetVertices() {
		if !pgraph.VertexContains(v, keep) {
			// wait for exit before starting new graph!
			v.SendEvent(event.EventExit, true, false)
			graph.DeleteVertex(v)
		}
	}

	for _, e := range c.Edges {
		if _, ok := lookup[util.FirstToUpper(e.From.Kind)]; !ok {
			return nil, fmt.Errorf("Can't find 'from' resource!")
		}
		if _, ok := lookup[util.FirstToUpper(e.To.Kind)]; !ok {
			return nil, fmt.Errorf("Can't find 'to' resource!")
		}
		if _, ok := lookup[util.FirstToUpper(e.From.Kind)][e.From.Name]; !ok {
			return nil, fmt.Errorf("Can't find 'from' name!")
		}
		if _, ok := lookup[util.FirstToUpper(e.To.Kind)][e.To.Name]; !ok {
			return nil, fmt.Errorf("Can't find 'to' name!")
		}
		graph.AddEdge(lookup[util.FirstToUpper(e.From.Kind)][e.From.Name], lookup[util.FirstToUpper(e.To.Kind)][e.To.Name], pgraph.NewEdge(e.Name))
	}

	return graph, nil
}
