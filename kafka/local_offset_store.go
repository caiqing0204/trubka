package kafka

import (
	"bytes"
	"encoding/gob"
	"os"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/peterbourgon/diskv"
	"github.com/pkg/errors"

	"github.com/xitonix/trubka/internal"
)

type progress struct {
	topic     string
	partition int32
	offset    int64
}

type localOffsetStore struct {
	db          *diskv.Diskv
	printer     internal.Printer
	wg          sync.WaitGroup
	writeErrors chan error
	in          chan *progress

	offsets map[string]map[int32]int64
}

func newLocalOffsetStore(printer internal.Printer, base string) (*localOffsetStore, error) {
	printer.Logf(internal.Verbose, "Initialising local offset store at %s", base)

	flatTransform := func(s string) []string { return []string{} }

	db := diskv.New(diskv.Options{
		BasePath:     base,
		Transform:    flatTransform,
		CacheSizeMax: 1024 * 1024,
	})

	return &localOffsetStore{
		db:          db,
		printer:     printer,
		writeErrors: make(chan error),
		in:          make(chan *progress, 100),
		offsets:     make(map[string]map[int32]int64),
	}, nil
}

func (s *localOffsetStore) start() {
	s.wg.Add(1)
	ticker := time.NewTicker(3 * time.Second)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-ticker.C:
				s.writeOffsetsToDisk()
			case p, more := <-s.in:
				if !more {
					ticker.Stop()
					s.printer.Log(internal.Verbose, "Flushing the offsets to disk.")
					s.writeOffsetsToDisk()
					return
				}
				_, ok := s.offsets[p.topic]
				if !ok {
					s.offsets[p.topic] = make(map[int32]int64)
				}
				s.offsets[p.topic][p.partition] = p.offset
			}
		}
	}()
}

// Returns the channel on which the write errors will be received.
// You must listen to this channel to avoid deadlock.
func (s *localOffsetStore) errors() <-chan error {
	return s.writeErrors
}

// Store saves the topic offset to the local disk.
func (s *localOffsetStore) Store(topic string, partition int32, offset int64) error {
	if offset == sarama.OffsetOldest || offset == sarama.OffsetNewest {
		return nil
	}
	s.in <- &progress{
		topic:     topic,
		partition: partition,
		offset:    offset,
	}
	return nil
}

// Query loads the offsets of all the available partitions from the local disk.
func (s *localOffsetStore) Query(topic string) (map[int32]int64, error) {
	offsets := make(map[int32]int64)
	val, err := s.db.Read(topic)
	if err != nil {
		if os.IsNotExist(err) {
			s.offsets[topic] = offsets
			return offsets, nil
		}
		return nil, err
	}

	buff := bytes.NewBuffer(val)
	dec := gob.NewDecoder(buff)
	err = dec.Decode(&offsets)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to deserialize the value from local offset store for topic %s", topic)
	}
	s.offsets[topic] = offsets
	return offsets, nil
}

func (s *localOffsetStore) close() {
	if s == nil || s.db == nil {
		return
	}
	s.printer.Log(internal.SuperVerbose, "Closing the offset store.")
	close(s.in)
	s.wg.Wait()
	close(s.writeErrors)
	s.printer.Log(internal.SuperVerbose, "The offset store has been closed successfully.")
}

func (s *localOffsetStore) writeOffsetsToDisk() {
	for topic, offsets := range s.offsets {
		buff := bytes.Buffer{}
		enc := gob.NewEncoder(&buff)
		toWrite := make(map[int32]int64)
		for p, o := range offsets {
			if o != sarama.OffsetNewest && o != sarama.OffsetOldest {
				toWrite[p] = o
			}
		}
		if len(toWrite) == 0 {
			return
		}
		err := enc.Encode(toWrite)
		if err != nil {
			s.writeErrors <- errors.Wrapf(err, "Failed to serialise the offsets of topic %s", topic)
		}
		s.printer.Logf(internal.SuperVerbose, "Writing the offset(s) of topic %s to the disk %v.", topic, toWrite)
		err = s.db.Write(topic, buff.Bytes())
		if err != nil {
			s.writeErrors <- errors.Wrapf(err, "Failed to write the offsets of topic %s to the disk %v", topic, toWrite)
		}
	}
}
