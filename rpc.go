// Copyright (C) 2016  The GoHBase Authors.  All rights reserved.
// This file is part of GoHBase.
// Use of this source code is governed by the Apache License 2.0
// that can be found in the COPYING file.

package gohbase

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/golang/protobuf/proto"
	"github.com/tsuna/gohbase/hrpc"
	"github.com/tsuna/gohbase/region"
	"github.com/tsuna/gohbase/zk"
	"golang.org/x/net/context"
)

// Constants
var (
	// Name of the meta region.
	metaTableName = []byte("hbase:meta")

	infoFamily = map[string][]string{
		"info": nil,
	}

	// ErrDeadline is returned when the deadline of a request has been exceeded
	ErrDeadline = errors.New("deadline exceeded")

	// ErrRegionUnavailable is returned when sending rpc to a region that is unavailable
	ErrRegionUnavailable = errors.New("region unavailable")

	// TableNotFound is returned when attempting to access a table that
	// doesn't exist on this cluster.
	TableNotFound = errors.New("table not found")

	// Default timeouts

	// How long to wait for a region lookup (either meta lookup or finding
	// meta in ZooKeeper).  Should be greater than or equal to the ZooKeeper
	// session timeout.
	regionLookupTimeout = 30 * time.Second

	backoffStart = 16 * time.Millisecond
)

func (c *client) sendRPC(rpc hrpc.Call) (proto.Message, error) {
	// Check the cache for a region that can handle this request
	var err error

	for {
		// block in case someone is updating regions.
		// for example someone is replacing a region with a new one,
		// we want to wait for that to finish so that we don't do
		// unnecessary region lookups in case that's our region.
		reg := c.getRegionFromCache(rpc.Table(), rpc.Key())
		if reg == nil {
			reg, err = c.findRegion(rpc.Context(), rpc.Table(), rpc.Key())
			if err != nil {
				return nil, err
			}
		}

		msg, err := c.sendRPCToRegion(rpc, reg)
		switch err {
		case ErrRegionUnavailable:
			if ch := reg.AvailabilityChan(); ch != nil {
				// The region is unavailable. Wait for it to become available,
				// a new region or for the deadline to be exceeded.
				select {
				case <-rpc.Context().Done():
					return nil, ErrDeadline
				case <-ch:
				}
			}
		default:
			return msg, err
		}
	}
}

func (c *client) sendRPCToRegion(rpc hrpc.Call, reg hrpc.RegionInfo) (proto.Message, error) {
	if reg.IsUnavailable() {
		return nil, ErrRegionUnavailable
	}

	rpc.SetRegion(reg)

	// Queue the RPC to be sent to the region
	client := reg.Client()
	if client == nil {
		// There was an error queueing the RPC.
		// Mark the region as unavailable.
		if reg.MarkUnavailable() {
			// If this was the first goroutine to mark the region as
			// unavailable, start a goroutine to reestablish a connection
			go c.reestablishRegion(reg)
		}
		return nil, ErrRegionUnavailable
	}

	client.QueueRPC(rpc)

	// Wait for the response
	var res hrpc.RPCResult
	select {
	case res = <-rpc.ResultChan():
	case <-rpc.Context().Done():
		return nil, ErrDeadline
	}

	// Check for errors
	switch res.Error.(type) {
	case region.RetryableError:
		// There's an error specific to this region, but
		// our region client is fine. Mark this region as
		// unavailable (as opposed to all regions sharing
		// the client), and start a goroutine to reestablish
		// it.
		if reg.MarkUnavailable() {
			go c.reestablishRegion(reg)
		}
		if reg != c.adminRegionInfo {
			// The client won't be in the clients cache if this is the admin region
			c.clients.del(reg)
		}
		return nil, ErrRegionUnavailable
	case region.UnrecoverableError:
		// If it was an unrecoverable error, the region client is
		// considered dead.
		if reg == c.adminRegionInfo {
			// If this is the admin client, mark the region
			// as unavailable and start up a goroutine to
			// reconnect if it wasn't already marked as such.
			if reg.MarkUnavailable() {
				go c.reestablishRegion(reg)
			}
		} else {
			// Else this is a normal region. Mark all the regions
			// sharing this region's client as unavailable, and start
			// a goroutine to reconnect for each of them.
			downregions := c.clients.clientDown(client)
			for _, downreg := range downregions {
				if downreg.MarkUnavailable() {
					downreg.SetClient(nil)
					go c.reestablishRegion(downreg)
				}
			}
		}

		// Fall through to the case of the region being unavailable,
		// which will result in blocking until it's available again.
		return nil, ErrRegionUnavailable
	default:
		// RPC was successfully sent, or an unknown type of error
		// occurred. In either case, return the results.
		return res.Msg, res.Error
	}
}

func (c *client) lookupRegion(ctx context.Context,
	table, key []byte) (hrpc.RegionInfo, string, uint16, error) {
	var reg hrpc.RegionInfo
	var host string
	var port uint16
	var err error
	backoff := backoffStart
	for {
		// If it takes longer than regionLookupTimeout, fail so that we can sleep
		lookupCtx, cancel := context.WithTimeout(ctx, regionLookupTimeout)
		if c.clientType == adminClient {
			host, port, err = c.zkLookup(lookupCtx, zk.Master)
			cancel()
			reg = c.adminRegionInfo
		} else if bytes.Compare(table, c.metaRegionInfo.Table()) == 0 {
			host, port, err = c.zkLookup(lookupCtx, zk.Meta)
			cancel()
			reg = c.metaRegionInfo
		} else {
			reg, host, port, err = c.metaLookup(lookupCtx, table, key)
			cancel()
			if err == TableNotFound {
				return nil, "", 0, err
			}
		}
		if err == nil {
			return reg, host, port, nil
		} else {
			log.Printf("Error looking up region for table=%q key=%q: %v",
				table, key, err)
		}
		// This will be hit if there was an error locating the region
		backoff, err = sleepAndIncreaseBackoff(ctx, backoff)
		if err != nil {
			return nil, "", 0, err
		}
	}
}

func (c *client) findRegion(ctx context.Context, table, key []byte) (hrpc.RegionInfo, error) {
	// The region was not in the cache, it
	// must be looked up in the meta table
	reg, host, port, err := c.lookupRegion(ctx, table, key)
	if err != nil {
		return nil, err
	}

	reg.MarkUnavailable()
	if reg != c.metaRegionInfo && reg != c.adminRegionInfo {
		// Check that the region wasn't added to
		// the cache while we were looking it up.
		if inCache, removed := c.regions.put(reg); inCache != reg {
			return inCache, nil
		} else {
			// remove clients
			for _, r := range removed {
				c.clients.del(r)
			}
		}
	}

	// Start a goroutine to connect to the region
	go c.establishRegion(reg, host, port)

	// Wait for the new region to become
	// available, and then send the RPC
	return reg, nil
}

// Searches in the regions cache for the region hosting the given row.
func (c *client) getRegionFromCache(table, key []byte) hrpc.RegionInfo {
	if c.clientType == adminClient {
		return c.adminRegionInfo
	} else if bytes.Equal(table, metaTableName) {
		return c.metaRegionInfo
	}
	regionName := createRegionSearchKey(table, key)
	_, region := c.regions.get(regionName)
	if region == nil || !bytes.Equal(table, region.Table()) {
		return nil
	}

	if len(region.StopKey()) != 0 &&
		// If the stop key is an empty byte array, it means this region is the
		// last region for this table and this key ought to be in that region.
		bytes.Compare(key, region.StopKey()) >= 0 {
		return nil
	}

	return region
}

// Creates the META key to search for in order to locate the given key.
func createRegionSearchKey(table, key []byte) []byte {
	metaKey := make([]byte, 0, len(table)+len(key)+3)
	metaKey = append(metaKey, table...)
	metaKey = append(metaKey, ',')
	metaKey = append(metaKey, key...)
	metaKey = append(metaKey, ',')
	// ':' is the first byte greater than '9'.  We always want to find the
	// entry with the greatest timestamp, so by looking right before ':'
	// we'll find it.
	metaKey = append(metaKey, ':')
	return metaKey
}

// metaLookup checks meta table for the region in which the given row key for the given table is.
func (c *client) metaLookup(ctx context.Context,
	table, key []byte) (hrpc.RegionInfo, string, uint16, error) {

	metaKey := createRegionSearchKey(table, key)
	rpc, err := hrpc.NewGetBefore(ctx, metaTableName, metaKey, hrpc.Families(infoFamily))
	if err != nil {
		return nil, "", 0, err
	}

	resp, err := c.Get(rpc)
	if err != nil {
		return nil, "", 0, err
	}
	if len(resp.Cells) == 0 {
		return nil, "", 0, TableNotFound
	}

	reg, host, port, err := region.ParseRegionInfo(resp)
	if err != nil {
		return nil, "", 0, err
	}
	if !bytes.Equal(table, reg.Table()) {
		// This would indicate a bug in HBase.
		return nil, "", 0, fmt.Errorf("wtf: meta returned an entry for the wrong table!"+
			"  Looked up table=%q key=%q got region=%s", table, key, reg)
	} else if len(reg.StopKey()) != 0 &&
		bytes.Compare(key, reg.StopKey()) >= 0 {
		// This would indicate a hole in the meta table.
		return nil, "", 0, fmt.Errorf("wtf: meta returned an entry for the wrong region!"+
			"  Looked up table=%q key=%q got region=%s", table, key, reg)
	}
	return reg, host, port, nil
}

func (c *client) reestablishRegion(reg hrpc.RegionInfo) {
	c.establishRegion(reg, "", 0)
}

func (c *client) establishRegion(reg hrpc.RegionInfo, host string, port uint16) {
	backoff := backoffStart
	var err error
	for {
		if host == "" && port == 0 {
			// need to look up region and address of the regionserver
			originalReg := reg
			// lookup region forever until we get it or we learn that it doesn't exist
			reg, host, port, err = c.lookupRegion(reg.Context(),
				originalReg.Table(), originalReg.StartKey())
			if err == TableNotFound {
				// region doesn't exist, delete it from caches
				c.regions.del(originalReg)
				c.clients.del(originalReg)
				originalReg.MarkAvailable()

				log.WithFields(log.Fields{
					"region": originalReg.String(),
					"err":    err,
				}).Info("region does not exist anymore")
				return
			} else if err == ErrDeadline {
				// region is dead
				originalReg.MarkAvailable()
				log.WithFields(log.Fields{
					"region": originalReg.String(),
					"err":    err,
				}).Info("region became dead while I was trying to reestablish it")
				return
			} else if err != nil {
				log.WithFields(log.Fields{
					"region": originalReg.String(),
					"err":    err,
				}).Fatal("unknown error occured when looking up region")
			}
			if !bytes.Equal(reg.Name(), originalReg.Name()) {
				// put new region and remove overlapping ones.
				// Should remove the original region as well.
				reg.MarkUnavailable()
				inCache, removed := c.regions.put(reg)
				if reg != inCache {
					// someone already added this region before us. Can happen
					// in a very rare case during a region merge.
					originalReg.MarkAvailable()
					return
				}
				for _, r := range removed {
					c.clients.del(r)
				}
				// let rpcs know that they can retry and either get the newly
				// added region from cache or lookup the one they need
				originalReg.MarkAvailable()
			} else {
				// same region, discard the looked up one
				reg = originalReg
			}
		}

		// connect to the region's regionserver
		if client, err := c.establishRegionClient(reg, host, port); err == nil {
			if c.clientType != adminClient {
				if existing := c.clients.put(client, reg); existing != client {
					// a client for this regionserver is already in cache, discard this one.
					client.Close()
					client = existing
				}
			}
			// set region client so that as soon as we mark it available,
			// concurrent readers are able to find the client
			reg.SetClient(client)
			reg.MarkAvailable()
			return
		} else if err == context.Canceled {
			// region is dead
			reg.MarkAvailable()
			return
		}

		// reset address because we weren't able to connect to it,
		// should look up again
		host, port = "", 0

		// This will be hit if there was an error connecting to the region
		backoff, err = sleepAndIncreaseBackoff(reg.Context(), backoff)
		if err != nil {
			// region is dead
			reg.MarkAvailable()
			return
		}
	}
}

func sleepAndIncreaseBackoff(ctx context.Context, backoff time.Duration) (time.Duration, error) {
	select {
	case <-time.After(backoff):
	case <-ctx.Done():
		return 0, ErrDeadline
	}
	// TODO: Revisit how we back off here.
	if backoff < 5000*time.Millisecond {
		return backoff * 2, nil
	} else {
		return backoff + 5000*time.Millisecond, nil
	}
}

func (c *client) establishRegionClient(reg hrpc.RegionInfo,
	host string, port uint16) (hrpc.RegionClient, error) {
	if c.clientType != adminClient {
		// if rpc is not for hbasemaster, check if client for regionserver
		// already exists
		if client := c.clients.checkForClient(host, port); client != nil {
			// There's already a client
			return client, nil
		}
	}

	var clientType region.ClientType
	if c.clientType == standardClient {
		clientType = region.RegionClient
	} else {
		clientType = region.MasterClient
	}
	clientCtx, cancel := context.WithTimeout(reg.Context(), regionLookupTimeout)
	defer cancel()
	return region.NewClient(clientCtx, host, port, clientType,
		c.rpcQueueSize, c.flushInterval)
}

// zkResult contains the result of a ZooKeeper lookup (when we're looking for
// the meta region or the HMaster).
type zkResult struct {
	host string
	port uint16
	err  error
}

// zkLookup asynchronously looks up the meta region or HMaster in ZooKeeper.
func (c *client) zkLookup(ctx context.Context, resource zk.ResourceName) (string, uint16, error) {
	// We make this a buffered channel so that if we stop waiting due to a
	// timeout, we won't block the zkLookupSync() that we start in a
	// separate goroutine.
	reschan := make(chan zkResult, 1)
	go func() {
		host, port, err := c.zkClient.LocateResource(resource)
		// This is guaranteed to never block as the channel is always buffered.
		reschan <- zkResult{host, port, err}
	}()
	select {
	case res := <-reschan:
		return res.host, res.port, res.err
	case <-ctx.Done():
		return "", 0, ErrDeadline
	}
}
