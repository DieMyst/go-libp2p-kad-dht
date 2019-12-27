package dht

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"

	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	pb "github.com/libp2p/go-libp2p-kad-dht/pb"
	kb "github.com/libp2p/go-libp2p-kbucket"
	"github.com/multiformats/go-base32"
	"github.com/multiformats/go-multihash"
)

func tryFormatLoggableKey(k string) (string, error) {
	if len(k) == 0 {
		return "", fmt.Errorf("loggableKey is empty")
	}
	var proto, cstr string
	if k[0] == '/' {
		// it's a path (probably)
		protoEnd := strings.IndexByte(k[1:], '/')
		if protoEnd < 0 {
			return k, fmt.Errorf("loggableKey starts with '/' but is not a path: %x", k)
		}
		proto = k[1 : protoEnd+1]
		cstr = k[protoEnd+2:]
	} else {
		proto = "provider"
		cstr = k
	}

	var encStr string
	c, err := cid.Cast([]byte(cstr))
	if err == nil {
		encStr = c.String()
	} else {
		encStr = base32.RawStdEncoding.EncodeToString([]byte(cstr))
	}

	return fmt.Sprintf("/%s/%s", proto, encStr), nil
}

func loggableKey(k string) logging.LoggableMap {
	newKey, err := tryFormatLoggableKey(k)
	if err != nil {
		logger.Debug(err)
	} else {
		k = newKey
	}

	return logging.LoggableMap{
		"key": k,
	}
}

func multihashLoggableKey(mh multihash.Multihash) logging.LoggableMap {
	return logging.LoggableMap{
		"multihash": base32.RawStdEncoding.EncodeToString(mh),
	}
}

// Kademlia 'node lookup' operation. Returns a channel of the K closest peers
// to the given key
func (dht *IpfsDHT) GetClosestPeers(ctx context.Context, key string) (<-chan peer.ID, error) {
	e := logger.EventBegin(ctx, "getClosestPeers", loggableKey(key))
	tablepeers := dht.routingTable.NearestPeers(kb.ConvertKey(key), AlphaValue)
	if len(tablepeers) == 0 {
		return nil, kb.ErrLookupFailure
	}

	out := make(chan peer.ID, dht.bucketSize)

	// since the query doesnt actually pass our context down
	// we have to hack this here. whyrusleeping isnt a huge fan of goprocess
	parent := ctx
	query := dht.newQuery(key, func(ctx context.Context, p peer.ID) (*dhtQueryResult, error) {
		// For DHT query command
		routing.PublishQueryEvent(parent, &routing.QueryEvent{
			Type: routing.SendingQuery,
			ID:   p,
		})

		pmes, err := dht.findPeerSingle(ctx, p, peer.ID(key))
		if err != nil {
			logger.Debugf("error getting closer peers: %s", err)
			return nil, err
		}
		peers := pb.PBPeersToPeerInfos(pmes.GetCloserPeers())

		// For DHT query command
		routing.PublishQueryEvent(parent, &routing.QueryEvent{
			Type:      routing.PeerResponse,
			ID:        p,
			Responses: peers,
		})

		return &dhtQueryResult{closerPeers: peers}, nil
	})

	go func() {
		defer close(out)
		defer e.Done()
		timedCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		// run it!
		res, err := query.Run(timedCtx, tablepeers)
		if err != nil {
			logger.Debugf("closestPeers query run error: %s", err)
		}

		if res != nil && res.queriedSet != nil {
			// refresh the cpl for this key as the query was successful
			dht.routingTable.ResetCplRefreshedAtForID(kb.ConvertKey(key), time.Now())

			sorted := kb.SortClosestPeers(res.queriedSet.Peers(), kb.ConvertKey(key))
			l := len(sorted)
			if l > dht.bucketSize {
				sorted = sorted[:dht.bucketSize]
			}

			for _, p := range sorted {
				out <- p
			}
		}
	}()

	return out, nil
}
