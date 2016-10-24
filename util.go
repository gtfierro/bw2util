// package: bw2util
// This package contains some helpful functions abstracting over bw2bind, providing some advanced functionality
package bw2util

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"

	"github.com/immesys/bw2/objects"
	"github.com/immesys/bw2/util"
	bw2 "github.com/immesys/bw2bind"
	"github.com/pkg/errors"
)

func fmtHash(hash []byte) string {
	return base64.URLEncoding.EncodeToString(hash)
}

// Wrapper for bw2 client that provides additional functionality
type Client struct {
	*bw2.BW2Client
	vk string
}

func NewClient(client *bw2.BW2Client, vk string) (*Client, error) {
	if len(vk) == 0 {
		return nil, fmt.Errorf("VK cannot be empty")
	}
	return &Client{client, vk}, nil
}

// Given a URI, returns the base64 encoding of the namespace VK that is the base of the URI
func (c *Client) GetNamespaceVK(uri string) (string, error) {
	parts := strings.Split(uri, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("Could not parse URI %s", uri)
	}
	head := parts[0]
	ro, _, err := c.ResolveRegistry(head)
	if err != nil {
		return "", err
	}
	f := reflect.ValueOf(ro).MethodByName("GetVK")
	nsvk := base64.URLEncoding.EncodeToString(f.Call([]reflect.Value{})[0].Bytes())
	return nsvk, nil
}

//TODO: get the overlap of all found dchains

// I want to subscribe to some broad pattern (e.g. scatch.ns/*/!meta/giles), but my access is distributed over
// several different different DOT chains. In order to do this, we first find *all* chains from the Namespace VK
// of the subscription URI to our own VK. For each of these chains (modulo any overlaps), we create a subscription
// manually specifying the primary access chain, then demux these subscriptions into a single channel which is returned
func (c *Client) MultiSubscribe(uri string) (chan *bw2.SimpleMessage, error) {
	// get NSVK for URI
	nsvk, err := c.GetNamespaceVK(uri)
	if err != nil {
		return nil, errors.Wrap(err, "Could not resolve namespace")
	}

	// build all of the chains we can use to subscribe
	dchains, err := c.FindDOTChains(nsvk)
	if err != nil {
		return nil, errors.Wrap(err, "Could not find DOT chains")
	}

	demuxed := make(chan *bw2.SimpleMessage, 10)

	for _, dchain := range dchains {
		// first form the actual subscription URI
		subURI := GetDChainURI(dchain, uri)
		go func(uri string, dchain *objects.DChain) {
			c, err := c.Subscribe(&bw2.SubscribeParams{
				URI:            subURI,
				AutoChain:      false,
				RoutingObjects: []objects.RoutingObject{dchain},
				ElaboratePAC:   bw2.ElaboratePartial,
			})
			if err != nil {
				fmt.Println(err)
				return
			}
			for msg := range c {
				demuxed <- msg
			}

		}(subURI, dchain)
		go func(uri string, dchain *objects.DChain) {
			c, err := c.Query(&bw2.QueryParams{
				URI:            subURI,
				AutoChain:      false,
				RoutingObjects: []objects.RoutingObject{dchain},
				ElaboratePAC:   bw2.ElaboratePartial,
			})
			if err != nil {
				fmt.Println(err)
				return
			}
			for msg := range c {
				demuxed <- msg
			}
		}(subURI, dchain)
	}

	return demuxed, nil
}

// finds valid access DOTs granted from the given VK
func (c *Client) findDOTsFromVK(fromvk string) ([]*objects.DOT, error) {
	var (
		retDOTs []*objects.DOT
	)
	dots, valids, err := c.FindDOTsFromVK(fromvk)
	if err != nil {
		return retDOTs, err
	}
	for i, ro := range dots {
		_dot, err := objects.NewDOT(ro.GetRONum(), ro.GetContent())
		// skip invalid DOTs
		if valids[i] != bw2.StateValid {
			continue
		}
		if err != nil {
			return retDOTs, err
		}
		dot := _dot.(*objects.DOT)
		if !dot.IsAccess() {
			continue
		}
		if permset := dot.GetPermissionSet(); !permset.CanConsume {
			continue
		}
		retDOTs = append(retDOTs, dot)
	}
	return retDOTs, nil
}

func (c *Client) FindDOTChains(namespace string) ([]*objects.DChain, error) {
	var (
		dchains []*objects.DChain
	)
	// get the list of lists of DOTs
	dotlists, err := c.findDOTChains(namespace, c.vk, namespace)
	if err != nil {
		return nil, err
	}
	// for every list, collapse it into a DChain object
	for _, chain := range dotlists {
		dchain, err := objects.CreateDChain(true, chain...)
		if err != nil {
			return nil, err
		}
		// skip the dchain if it is invalid or isn't an access chain
		if !dchain.IsAccess() || !dchain.CheckAllSigs() {
			continue
		}

		dchains = append(dchains, dchain)
	}
	return dchains, err
}

// find/build lists of DOTs between the two VKs on the given namespace
func (c *Client) findDOTChains(fromvk, findvk, namespace string) ([][]*objects.DOT, error) {
	var (
		chains [][]*objects.DOT
	)
	dots, err := c.findDOTsFromVK(fromvk)
	if err != nil {
		return chains, errors.Wrap(err, "Could not find DOTS from vk")
	}
	for _, dot := range dots {
		var our_chain []*objects.DOT
		// check if the DOT is granted on the right namespace
		mvk := fmtHash(dot.GetAccessURIMVK())
		if mvk != namespace {
			continue
		}
		// add dot to the current chain
		our_chain = append(our_chain, dot)

		// check if the DOT is granted to our VK. If it is, we terminate this branch of
		// the search
		recvVK := fmtHash(dot.GetReceiverVK())
		if recvVK == findvk {
			chains = append(chains, our_chain)
			continue
		}

		// otherwise, we continue our search
		recursive_chains, err := c.findDOTChains(recvVK, findvk, namespace)
		if err != nil {
			return chains, err
		}
		for _, chain := range recursive_chains {
			chains = append(chains, append(our_chain, chain...))
		}
	}
	return chains, nil
}

// given a dchain and a URI, return the broadest URI you can actually
// subscribe to using the dchain. Assumes the DChain is elaborated (i.e. it has
// all of its DOTs populated)
func GetDChainURI(dchain *objects.DChain, uri string) string {
	subURI := GetURISuffix(uri)
	ns := strings.Split(uri, "/")[0]
	// collapse the DOT to get the actual subscription URI
	for i := 0; i < dchain.NumHashes(); i++ {
		dot := dchain.GetDOT(i)
		newURI, overlap := util.RestrictBy(dot.GetAccessURISuffix(), subURI)
		// if it don't overlap, don't use it
		if !overlap {
			return ""
		}
		subURI = newURI
	}

	return ns + "/" + subURI
}

// Returns the URI that's not the namespace
func GetURISuffix(uri string) string {
	return strings.Join(strings.Split(uri, "/")[1:], "/")
}
