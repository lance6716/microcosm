package resource

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"go.uber.org/zap"

	"github.com/hanfei1991/microcosm/client"
	"github.com/hanfei1991/microcosm/pb"
)

type ID string

// FileWriter supports two methods:
// - Write: classical IO API.
// - Close: close the file and completes the upload if needed.
type FileWriter interface {
	storage.ExternalFileWriter
}

type fileWriter struct {
	storage.ExternalFileWriter
	resourceID ID
	executorID string
	masterCli  client.MasterClient
}

func (f *fileWriter) Close(ctx context.Context) error {
	err := f.ExternalFileWriter.Close(ctx)
	if err != nil {
		return err
	}
	// failing here will generate trash files, need clean it
	resp, err := f.masterCli.PersistResource(ctx, &pb.PersistResourceRequest{
		ExecutorId: f.executorID,
		ResourceId: string(f.resourceID),
	})
	if err != nil {
		return errors.New(resp.Err.String())
	}
	return nil
}

// Proxy is assigned to a worker so worker can create the resource files and notify
// the dataflow engine.
type Proxy interface {
	// ID identifies the resources that one worker created, to provide cross-worker
	// constrains.
	ID() ID
	// CreateFile can create a FileWriter for writing at the given path under current
	// ID.
	CreateFile(ctx context.Context, path string) (FileWriter, error)
}

type proxy struct {
	resourceID ID
	executorID string
	masterCli  *ignoreAfterSuccClient // we only need one request for one resource
	storage    storage.ExternalStorage
}

func (p proxy) ID() ID {
	return p.resourceID
}

func (p proxy) CreateFile(ctx context.Context, path string) (FileWriter, error) {
	writer, err := p.storage.Create(ctx, path)
	if err != nil {
		return nil, err
	}
	return &fileWriter{
		ExternalFileWriter: writer,
		resourceID:         p.resourceID,
		executorID:         p.executorID,
		masterCli:          p.masterCli,
	}, nil
}

func newProxy(ctx context.Context, pathPrefix, id string) (*proxy, error) {
	storagePath := filepath.Join(pathPrefix, id)
	// only support local disk now
	backend, err := storage.ParseBackend(storagePath, nil)
	if err != nil {
		return nil, err
	}
	s, err := storage.New(ctx, backend, nil)
	if err != nil {
		return nil, err
	}
	return &proxy{
		resourceID: ID(id),
		storage:    s,
	}, nil
}

// Broker is singleton per executor, it communicates with Manager of server master.
type Broker struct {
	// map[ID]struct{}
	allocated sync.Map

	executorID string
	pathPrefix string
	masterCli  client.MasterClient // nil when in test
}

func NewBroker(executorID, pathPrefix string, masterCli client.MasterClient) *Broker {
	return &Broker{
		executorID: executorID,
		pathPrefix: pathPrefix,
		masterCli:  masterCli,
	}
}

var MockBroker = Broker{
	pathPrefix: "resources",
}

func (b *Broker) NewProxyForWorker(ctx context.Context, id string) (Proxy, error) {
	p, err := newProxy(ctx, b.pathPrefix, id)
	if err != nil {
		return nil, err
	}
	p.masterCli = &ignoreAfterSuccClient{
		MasterClient: b.masterCli,
	}
	p.executorID = b.executorID
	b.allocated.Store(ID(id), struct{}{})
	return p, nil
}

func (b *Broker) Remove(id string) {
	// TODO: what if the worker is still using the resource by proxy?
	b.allocated.Delete(ID(id))
}

func (b *Broker) AllocatedIDs() []string {
	var ids []string
	b.allocated.Range(func(k, v interface{}) bool {
		ids = append(ids, string(k.(ID)))
		return true
	})
	return ids
}

func NewMockProxy(id string) Proxy {
	p, err := newProxy(context.TODO(), "unit_test_resources", id)
	if err != nil {
		log.L().Panic("failed in NewMockProxy",
			zap.String("resourceID", id),
			zap.Error(err))
	}
	return p
}
