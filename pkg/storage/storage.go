package storage

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/dougbtv/whereabouts/pkg/allocate"
	"github.com/dougbtv/whereabouts/pkg/logging"
	"github.com/dougbtv/whereabouts/pkg/types"
)

var (

	// DatastoreRetries defines how many retries are attempted when updating the Pool
	DatastoreRetries = 100
)

// IPPool is the interface that represents an manageable pool of allocated IPs
type IPPool interface {
	Allocations() []types.IPReservation
	Update(ctx context.Context, reservations []types.IPReservation) error
}

// Store is the interface that wraps the basic IP Allocation methods on the underlying storage backend
type Store interface {
	GetIPPool(ctx context.Context, ipRange string) (IPPool, error)
	Status(ctx context.Context) error
	Close(ctx context.Context) error
}

// IPManagement manages ip allocation and deallocation from a storage perspective
func IPManagement(mode int, ipamConf types.IPAMConfig, containerID string, podRef string) (net.IPNet, error) {

	logging.Debugf("IPManagement -- mode: %v / host: %v / containerID: %v / podRef: %v", mode, ipamConf.EtcdHost, containerID, podRef)

	var newip net.IPNet
	// Skip invalid modes
	switch mode {
	case types.Allocate, types.Deallocate:
	default:
		return newip, fmt.Errorf("Got an unknown mode passed to IPManagement: %v", mode)
	}

	ctx, acquireCancel := context.WithTimeout(context.Background(), time.Duration(ipamConf.LockRequestTimeout)*time.Second)
	defer acquireCancel()

	var ipam Store
	var pool IPPool
	var err error
	switch ipamConf.Datastore {
	case types.DatastoreETCD:
		ipam, err = NewETCDIPAM(ctx, ipamConf)
	case types.DatastoreKubernetes:
		ipam, err = NewKubernetesIPAM(ctx, containerID, ipamConf)
	}
	if err != nil {
		logging.Errorf("IPAM %s client initialization error: %v", ipamConf.Datastore, err)
		return newip, fmt.Errorf("IPAM %s client initialization error: %v", ipamConf.Datastore, err)
	}
	defer func() {
		ctx, releaseCancel := context.WithTimeout(context.Background(), time.Duration(ipamConf.LockRequestTimeout)*time.Second)
		err = ipam.Close(ctx)
		if err != nil {
			logging.Errorf("error in closing ipam pool %v", err)
		}
		releaseCancel()
	}()

	ctx, ipPoolOpCancel := context.WithTimeout(context.Background(), time.Duration(ipamConf.RequestTimeout)*time.Second)
	defer ipPoolOpCancel()

	// Check our connectivity first
	if err := ipam.Status(ctx); err != nil {
		logging.Errorf("IPAM connectivity error: %v", err)
		return newip, err
	}

	// handle the ip add/del until successful
RETRYLOOP:
	for j := 0; j < DatastoreRetries; j++ {
		select {
		case <-ctx.Done():
			// return last available newip and err
			logging.Errorf("context is done for ip pool %s, returning last ip %s: error %v", ipamConf.Range, newip.String(), err)
			return newip, err
		default:
			// retry the IPAM loop if the context has not been cancelled
		}

		pool, err = ipam.GetIPPool(ctx, ipamConf.Range)
		if err != nil {
			logging.Errorf("IPAM error reading pool allocations (attempt: %d): %v", j, err)
			if e, ok := err.(temporary); ok && e.Temporary() {
				continue
			}
			return newip, err
		}

		reservelist := pool.Allocations()
		var updatedreservelist []types.IPReservation
		switch mode {
		case types.Allocate:
			newip, updatedreservelist, err = allocate.AssignIP(ipamConf, reservelist, containerID, podRef)
			if err != nil {
				logging.Errorf("Error assigning IP: %v", err)
				return newip, err
			}
		case types.Deallocate:
			updatedreservelist, err = allocate.DeallocateIP(ipamConf.Range, reservelist, containerID)
			if err != nil {
				logging.Errorf("Error deallocating IP: %v", err)
				return newip, err
			}
		}

		err = pool.Update(ctx, updatedreservelist)
		if err != nil {
			logging.Errorf("IPAM error updating pool %s (attempt: %d): %v", ipamConf.Range, j, err)
			if e, ok := err.(temporary); ok && e.Temporary() {
				logging.Errorf("IPAM error is temporary for pool %s: %v, retrying", ipamConf.Range, err)
				continue
			}
			break RETRYLOOP
		}
		break RETRYLOOP
	}

	return newip, err
}
