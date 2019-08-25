package util

import (
	"encoding/json"
	"sync"
)

const DEFAULT_SHARD_COUNT = 32

// A "thread" safe map of type string:Anything.
// To avoid lock bottlenecks this map is dived to several (DEFAULT_SHARD_COUNT) map shards.
type ConcurrentMapString struct {
	tables      []*concurrentMapSharedString
	shard_count int
}

// A "thread" safe string to anything map.
type concurrentMapSharedString struct {
	items map[string]interface{}
	sync.RWMutex // Read Write mutex, guards access to internal map.
}

// Creates a new concurrent map.
func NewConcurrentMapString(shardCount int) *ConcurrentMapString {
	if shardCount <= 0 {
		shardCount = DEFAULT_SHARD_COUNT
	}
	rect := ConcurrentMapString{
		shard_count: shardCount,
	}
	m := make([]*concurrentMapSharedString, shardCount)
	for i := 0; i < shardCount; i++ {
		m[i] = &concurrentMapSharedString{items: make(map[string]interface{})}
	}
	rect.tables = m
	return &rect
}

// Returns shard under given key
func (m *ConcurrentMapString) GetShard(key string) *concurrentMapSharedString {
	return m.tables[uint(fnv32(key))%uint(m.shard_count)]
}

func (m *ConcurrentMapString) MSet(data map[string]interface{}) {
	for key, value := range data {
		shard := m.GetShard(key)
		shard.Lock()
		shard.items[key] = value
		shard.Unlock()
	}
}

// Sets the given value under the specified key.
func (m *ConcurrentMapString) Set(key string, value interface{}) {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	shard.items[key] = value
	shard.Unlock()
}

// Callback to return new element to be inserted into the map
// It is called while lock is held, therefore it MUST NOT
// try to access other keys in same map, as it can lead to deadlock since
// Go sync.RWLock is not reentrant
// 回调返回待插入到 map 中的新元素
// 这个函数当且仅当在读写锁被锁定的时候才会被调用，因此一定不允许再去尝试读取同一个 map 中的其他 key 值。因为这样会导致线程死锁。死锁的原因是 Go 中 sync.RWLock 是不可重入的。
type UpsertCb func(exist bool, valueInMap interface{}, newValue interface{}) interface{}

// Insert or Update - updates existing element or inserts a new one using UpsertCb
func (m *ConcurrentMapString) Upsert(key string, value interface{}, cb UpsertCb) (res interface{}) {
	shard := m.GetShard(key)
	shard.Lock()
	v, ok := shard.items[key]
	res = cb(ok, v, value)
	shard.items[key] = res
	shard.Unlock()
	return res
}

// Sets the given value under the specified key if no value was associated with it.
func (m *ConcurrentMapString) SetIfAbsent(key string, value interface{}) bool {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	_, ok := shard.items[key]
	if !ok {
		shard.items[key] = value
	}
	shard.Unlock()
	return !ok
}

// Retrieves an element from map under given key.
func (m *ConcurrentMapString) Get(key string) (interface{}, bool) {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// Get item from shard.
	val, ok := shard.items[key]
	shard.RUnlock()
	return val, ok
}

// Returns the number of elements within the map.
func (m *ConcurrentMapString) Count() int {
	count := 0
	for i := 0; i < m.shard_count; i++ {
		shard := m.tables[i]
		shard.RLock()
		count += len(shard.items)
		shard.RUnlock()
	}
	return count
}

// Looks up an item under specified key
func (m *ConcurrentMapString) Has(key string) bool {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// See if element is within shard.
	_, ok := shard.items[key]
	shard.RUnlock()
	return ok
}

// Removes an element from the map.
func (m *ConcurrentMapString) Remove(key string) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	delete(shard.items, key)
	shard.Unlock()
}

// Removes an element from the map and returns it
func (m *ConcurrentMapString) Pop(key string) (v interface{}, exists bool) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	v, exists = shard.items[key]
	delete(shard.items, key)
	shard.Unlock()
	return v, exists
}

// Checks if map is empty.
func (m *ConcurrentMapString) IsEmpty() bool {
	return m.Count() == 0
}

// Used by the Iter & IterBuffered functions to wrap two variables together over a channel,
type TupleString struct {
	Key string
	Val interface{}
}

// Returns an iterator which could be used in a for range loop.
//
// Deprecated: using IterBuffered() will get a better performence
func (m *ConcurrentMapString) Iter() <-chan TupleString {
	chans := snapshot(m)
	ch := make(chan TupleString)
	go fanIn(chans, ch)
	return ch
}

// Returns a buffered iterator which could be used in a for range loop.
func (m *ConcurrentMapString) IterBuffered() <-chan TupleString {
	chans := snapshot(m)
	total := 0
	for _, c := range chans {
		total += cap(c)
	}
	ch := make(chan TupleString, total)
	go fanIn(chans, ch)
	return ch
}

// Returns a array of channels that contains elements in each shard,
// which likely takes a snapshotUint32 of `m`.
// It returns once the size of each buffered channel is determined,
// before all the channels are populated using goroutines.
func snapshot(m *ConcurrentMapString) (chans []chan TupleString) {
	chans = make([]chan TupleString, m.shard_count)
	wg := sync.WaitGroup{}
	wg.Add(m.shard_count)
	// Foreach shard.
	for index, shard := range m.tables {
		go func(index int, shard *concurrentMapSharedString) { //注意：在子协程中使用for range生成的变量时一定作为参数传给子协程
			// Foreach key, value pair.
			shard.RLock()
			chans[index] = make(chan TupleString, len(shard.items))
			wg.Done()
			for key, val := range shard.items {
				chans[index] <- TupleString{key, val}
			}
			shard.RUnlock()
			close(chans[index])
		}(index, shard)
	}
	wg.Wait()
	return chans
}

// fanInuint32 reads elements from channels `chans` into channel `out`
func fanIn(chans []chan TupleString, out chan TupleString) {
	wg := sync.WaitGroup{}
	wg.Add(len(chans))
	for _, ch := range chans {
		go func(ch chan TupleString) { //注意：在子协程中使用for range生成的变量时一定作为参数传给子协程
			for t := range ch {
				out <- t
			}
			wg.Done()
		}(ch)
	}
	wg.Wait()
	close(out)
}

// Returns all items as map[string]interface{}
func (m *ConcurrentMapString) Items() map[string]interface{} {
	tmp := make(map[string]interface{})

	// Insert items to temporary map.
	for item := range m.IterBuffered() {
		tmp[item.Key] = item.Val
	}

	return tmp
}

// Iterator callback,called for every key,value found in
// maps. RLock is held for all calls for a given shard
// therefore callback sess consistent view of a shard,
// but not across the shards
type IterCb func(key string, v interface{})

// Callback based iterator, cheapest way to read
// all elements in a map.
func (m *ConcurrentMapString) IterCb(fn IterCb) {
	for idx := range m.tables {
		shard := (m.tables)[idx]
		shard.RLock()
		for key, value := range shard.items {
			fn(key, value)
		}
		shard.RUnlock()
	}
}

// Return all keys as []string
func (m *ConcurrentMapString) Keys() []string {
	count := m.Count()
	ch := make(chan string, count)
	go func() {
		// 遍历所有的 shard.
		wg := sync.WaitGroup{}
		wg.Add(m.shard_count)
		for _, shard := range m.tables {
			go func(shard *concurrentMapSharedString) { //注意：在子协程中使用for range生成的变量时一定作为参数传给子协程
				// 遍历所有的 key, value 键值对.
				shard.RLock()
				for key := range shard.items {
					ch <- key
				}
				shard.RUnlock()
				wg.Done()
			}(shard)
		}
		wg.Wait()
		close(ch)
	}()

	// 生成 keys 数组，存储所有的 key
	keys := make([]string, 0, count)
	for k := range ch {
		keys = append(keys, k)
	}
	return keys
}

//Reviles ConcurrentMapString "private" variables to json marshal.
func (m *ConcurrentMapString) MarshalJSON() ([]byte, error) {
	// Create a temporary map, which will hold all item spread across shards.
	tmp := make(map[string]interface{})

	// Insert items to temporary map.
	for item := range m.IterBuffered() {
		tmp[item.Key] = item.Val
	}
	return json.Marshal(tmp)
}

func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime32 = uint32(16777619)
	for i := 0; i < len(key); i++ {
		hash *= prime32
		hash ^= uint32(key[i])
	}
	return hash
}

// Concurrent map uses Interface{} as its value, therefor JSON Unmarshal
// will probably won't know which to type to unmarshal into, in such case
// we'll end up with a value of type map[string]interface{}, In most cases this isn't
// out value type, this is why we've decided to remove this functionality.

// func (m *ConcurrentMapString) UnmarshalJSON(b []byte) (err error) {
// 	// Reverse process of Marshal.

// 	tmp := make(map[string]interface{})

// 	// Unmarshal into a single map.
// 	if err := json.Unmarshal(b, &tmp); err != nil {
// 		return nil
// 	}

// 	// foreach key,value pair in temporary map insert into our concurrent map.
// 	for key, val := range tmp {
// 		m.Set(key, val)
// 	}
// 	return nil
// }

type MyMap struct {
	sync.Mutex
	m map[string]interface{}
}

var myMap *MyMap

func init() {
	myMap = &MyMap{
		m: make(map[string]interface{}, 100),
	}
}

func (myMap *MyMap) BuiltinMapStore(k string, v interface{}) {
	myMap.Lock()
	defer myMap.Unlock()
	myMap.m[k] = v
}

func (myMap *MyMap) BuiltinMapLookup(k string) interface{} {
	myMap.Lock()
	defer myMap.Unlock()
	if v, ok := myMap.m[k]; !ok {
		return -1
	} else {
		return v
	}
}

func (myMap *MyMap) BuiltinMapDelete(k string) {
	myMap.Lock()
	defer myMap.Unlock()
	if _, ok := myMap.m[k]; !ok {
		return
	} else {
		delete(myMap.m, k)
	}
}
