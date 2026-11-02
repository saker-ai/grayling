package rpc

import (
	"sync"
	"testing"
)

type graylingClient struct {
	IRpcClient
	rpc *RpcClient
}

func (c *graylingClient) GetRpcClient() *RpcClient { return c.rpc }

type graylingConnection struct {
	IConnection
	id string
}

func (c *graylingConnection) getConnectionId() string { return c.id }
func (c *graylingConnection) close()                  {}

func TestGraylingConcurrentConnectionAndRegistry(t *testing.T) {
	r := &RpcClient{name: "grayling-race-regression", eventChan: make(chan ConnectionEvent, 128)}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range r.eventChan {
		}
	}()
	r.setCurrentConnection(&graylingConnection{id: "initial"})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 5000; n++ {
				r.setCurrentConnection(&graylingConnection{id: "new"})
				connection := r.getCurrentConnection()
				if connection == nil || connection.getConnectionId() == "" {
					t.Error("unpublished connection")
				}
				cMux.Lock()
				clientMap[r.name] = &graylingClient{rpc: r}
				cMux.Unlock()
				_ = getClient(r.name)
				_ = getAllClient()
				r.Shutdown()
			}
		}()
	}
	wg.Wait()
	close(r.eventChan)
	<-drained
}
