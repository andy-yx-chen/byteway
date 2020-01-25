package cypress

import "math/rand"

import "context"

import "reflect"

import "errors"

import "database/sql"

// MyCluster MySQL based database cluster, up to 32 physical partitions,
// for cluster that requires more than 32 partitions, consider tidb or
// similar distributed database
type MyCluster struct {
	master          *DbAccessor
	partitions      []*DbAccessor
	unknownResolver *unknownStateTxnResolver
	idGen           UniqueIDGenerator
	partitionCalc   PartitionCalculator
	rand            *rand.Rand
}

// NewMyCluster creates an instance of MyCluster
func NewMyCluster(master *DbAccessor, partitions []*DbAccessor, txnTimeout int, idGen UniqueIDGenerator, partitionCalc PartitionCalculator) *MyCluster {
	if idGen == nil {
		idGen = NewDbUniqueIDGenerator(master)
	}

	if partitionCalc == nil {
		partitionCalc = PartitionCalculateFunc(CalculateMd5PartitionKey)
	}

	return &MyCluster{
		master:     master,
		partitions: partitions,
		unknownResolver: &unknownStateTxnResolver{
			master:     master,
			partitions: partitions,
			txnTimeout: txnTimeout,
		},
		idGen:         idGen,
		partitionCalc: partitionCalc,
		rand:          rand.New(rand.NewSource(GetEpochMillis())),
	}
}

// GetAllPartitions gets all physical partitions in cluster
func (cluster *MyCluster) GetAllPartitions() []*DbAccessor {
	return cluster.partitions
}

// GetAnyPartition gets a random partition from physical partitions
func (cluster *MyCluster) GetAnyPartition() *DbAccessor {
	return cluster.partitions[cluster.rand.Intn(len(cluster.partitions))]
}

// GetUniqueIDByName gets a unique id from cluster for the given entity name and partition
func (cluster *MyCluster) GetUniqueIDByName(ctx context.Context, entityName string, partition int32) (int64, error) {
	uniqueID, err := cluster.idGen.NextUniqueID(ctx, entityName, partition)
	if err != nil {
		return -1, err
	}

	return uniqueID.Value, nil
}

// GetUniqueID gets a unique id for the given entity prototype
func (cluster *MyCluster) GetUniqueID(ctx context.Context, entity interface{}, partition int32) (int64, error) {
	descriptor := GetOrCreateEntityDescriptor(reflect.TypeOf(entity))
	return cluster.GetUniqueIDByName(ctx, descriptor.tableName, partition)
}

func (cluster *MyCluster) getPartitionFromEntity(descriptor *EntityDescriptor, entityValue *reflect.Value) (int32, error) {
	if descriptor.partitionKey == nil {
		return -1, errors.New("No partition key defined")
	}

	if descriptor.key == nil || !descriptor.key.autoGen {
		return -1, errors.New("No key defined or key is tagged with nogen")
	}

	if entityValue.Kind() == reflect.Ptr {
		v := entityValue.Elem()
		entityValue = &v
	}

	partitionKey := entityValue.FieldByIndex(descriptor.partitionKey.field.Index).Interface()
	strValue, ok := partitionKey.(string)

	var partition int32 = -1
	if ok {
		partition = cluster.partitionCalc.GetPartition(strValue)
	} else if intValue, ok := partitionKey.(int64); ok {
		partition = GetPartitionKey(intValue)
	}

	if partition == -1 {
		return -1, errors.New("Not able to get partition for entity")
	}

	return partition, nil
}

func (cluster *MyCluster) generateKeyForEntity(ctx context.Context, descriptor *EntityDescriptor, partition int32, entityValue *reflect.Value) error {
	if descriptor.key != nil && descriptor.key.autoGen {
		uniqueID, err := cluster.idGen.NextUniqueID(ctx, descriptor.tableName, partition)
		if err != nil {
			return err
		}

		if entityValue.Kind() == reflect.Ptr {
			v := entityValue.Elem()
			entityValue = &v
		}

		entityValue.FieldByIndex(descriptor.key.field.field.Index).SetInt(uniqueID.Value)
	}

	return nil
}

func (cluster *MyCluster) getEntityKey(descriptor *EntityDescriptor, entityValue *reflect.Value) (int64, error) {
	if descriptor.key == nil {
		return -1, errors.New("no key defined")
	}

	if entityValue.Kind() == reflect.Ptr {
		v := entityValue.Elem()
		entityValue = &v
	}

	id, ok := entityValue.FieldByIndex(descriptor.key.field.field.Index).Interface().(int64)
	if !ok {
		return -1, errors.New("invalid key value")
	}

	return id, nil
}

// GenerateKey generates and assign an unique id for the given entity
func (cluster *MyCluster) GenerateKey(ctx context.Context, entity interface{}) error {
	descriptor := GetOrCreateEntityDescriptor(reflect.TypeOf(entity))
	value := reflect.ValueOf(entity)
	partition, err := cluster.getPartitionFromEntity(descriptor, &value)
	if err != nil {
		return err
	}

	return cluster.generateKeyForEntity(ctx, descriptor, partition, &value)
}

// GetDbAccessor get DbAccessor by partition
func (cluster *MyCluster) GetDbAccessor(partition int32) *DbAccessor {
	return cluster.partitions[int(partition)%len(cluster.partitions)]
}

// GetDbAccessorByKey get DbAccessor by partition key
func (cluster *MyCluster) GetDbAccessorByKey(partitionKey string) *DbAccessor {
	partition := cluster.partitionCalc.GetPartition(partitionKey)
	return cluster.GetDbAccessor(partition)
}

// GetDbAccessorByID get DbAccessor by unique ID
func (cluster *MyCluster) GetDbAccessorByID(id int64) *DbAccessor {
	return cluster.GetDbAccessor(GetPartitionKey(id))
}

// InsertAt insert entity to specific partition
func (cluster *MyCluster) InsertAt(ctx context.Context, partition int32, entity interface{}) error {
	descriptor := GetOrCreateEntityDescriptor(reflect.TypeOf(entity))
	value := reflect.ValueOf(entity)
	err := cluster.generateKeyForEntity(ctx, descriptor, partition, &value)
	if err != nil {
		return err
	}

	_, err = cluster.GetDbAccessor(partition).Insert(ctx, entity)
	return err
}

// CreateTransaction creates a cluster transaction
func (cluster *MyCluster) CreateTransaction(ctx context.Context) (*MyClusterTxn, error) {
	insertResult, err := cluster.master.Execute(ctx, "insert into `cluster_txn`(`state`, `timestamp`) values (?, ?)", ClusterTxnStateNone, GetEpochMillis())
	if err != nil {
		return nil, err
	}

	txnID, err := insertResult.LastInsertId()
	if err != nil {
		return nil, err
	}

	return newMyClusterTxn(ctx, txnID, cluster.master, cluster.partitions, cluster.idGen, cluster.unknownResolver), nil
}

// InsertToAll insert entity to all partitions
func (cluster *MyCluster) InsertToAll(ctx context.Context, entity interface{}) error {
	txn, err := cluster.CreateTransaction(ctx)
	if err != nil {
		return err
	}

	defer txn.Close()
	for i := 0; i < len(cluster.partitions); i = i + 1 {
		_, err := txn.InsertAt(int32(i), entity)
		if err != nil {
			txn.MarkAsFaulted()
			return err
		}
	}

	return nil
}

// UpdateToAll update entity to all partitions
func (cluster *MyCluster) UpdateToAll(ctx context.Context, entity interface{}) error {
	txn, err := cluster.CreateTransaction(ctx)
	if err != nil {
		return err
	}

	defer txn.Close()
	for i := 0; i < len(cluster.partitions); i = i + 1 {
		_, err := txn.UpdateAt(int32(i), entity)
		if err != nil {
			txn.MarkAsFaulted()
			return err
		}
	}

	return nil
}

// ExecuteOnAll execute query on all partitions
func (cluster *MyCluster) ExecuteOnAll(ctx context.Context, query string, args ...interface{}) error {
	txn, err := cluster.CreateTransaction(ctx)
	if err != nil {
		return err
	}

	defer txn.Close()
	for i := 0; i < len(cluster.partitions); i = i + 1 {
		t, err := txn.GetTxnByPartition(int32(i))
		if err != nil {
			txn.MarkAsFaulted()
			return err
		}

		_, err = t.Execute(query, args...)
		if err != nil {
			txn.MarkAsFaulted()
			return err
		}
	}

	return nil
}

// Delete delete the given entity
func (cluster *MyCluster) Delete(ctx context.Context, entity interface{}) (sql.Result, error) {
	descriptor := GetOrCreateEntityDescriptor(reflect.TypeOf(entity))
	value := reflect.ValueOf(entity)
	id, err := cluster.getEntityKey(descriptor, &value)
	if err != nil {
		return nil, err
	}

	return cluster.GetDbAccessorByID(id).Delete(ctx, entity)
}

// InsertByKey insert entity to db based on given partition key
func (cluster *MyCluster) InsertByKey(ctx context.Context, partitionKey string, entity interface{}) error {
	return cluster.InsertAt(ctx, cluster.partitionCalc.GetPartition(partitionKey), entity)
}

// Insert insert the entity to db based on the tagged partition key
func (cluster *MyCluster) Insert(ctx context.Context, entity interface{}) error {
	descriptor := GetOrCreateEntityDescriptor(reflect.TypeOf(entity))
	value := reflect.ValueOf(entity)
	partition, err := cluster.getPartitionFromEntity(descriptor, &value)
	if err != nil {
		return err
	}

	err = cluster.generateKeyForEntity(ctx, descriptor, partition, &value)
	if err != nil {
		return err
	}

	_, err = cluster.GetDbAccessor(partition).Insert(ctx, entity)
	return err
}

// Update update the entity based on the key in the entity
func (cluster *MyCluster) Update(ctx context.Context, entity interface{}) error {
	descriptor := GetOrCreateEntityDescriptor(reflect.TypeOf(entity))
	value := reflect.ValueOf(entity)
	id, err := cluster.getEntityKey(descriptor, &value)
	if err != nil {
		return err
	}

	_, err = cluster.GetDbAccessorByID(id).Update(ctx, entity)
	return err
}

// UpdateAt update entity at the specific partition
func (cluster *MyCluster) UpdateAt(ctx context.Context, partition int32, entity interface{}) error {
	_, err := cluster.GetDbAccessor(partition).Update(ctx, entity)
	return err
}

// QueryOne query one entity with the given mapper
func (cluster *MyCluster) QueryOne(ctx context.Context, query string, mapper RowMapper, aggregator func(interface{}, interface{}) interface{}, args ...interface{}) (interface{}, error) {
	if aggregator == nil {
		return nil, errors.New("aggregator cannot be nil")
	}

	values := make([]interface{}, 0)
	for i := 0; i < len(cluster.partitions); i++ {
		value, err := cluster.partitions[i].QueryOne(ctx, query, mapper, args...)
		if err != nil {
			return nil, err
		}

		if value != nil {
			values = append(values, value)
		}
	}

	var result interface{} = nil
	for _, v := range values {
		if result == nil {
			result = v
		} else {
			result = aggregator(result, v)
		}
	}

	return result, nil
}

// GetOne query one entity by the given prototype
func (cluster *MyCluster) GetOne(ctx context.Context, prototype interface{}, query string, aggregator func(interface{}, interface{}) interface{}, args ...interface{}) (interface{}, error) {
	return cluster.QueryOne(ctx, query, NewSmartMapper(prototype), aggregator, args...)
}

// QueryAll query all entities from all partitions by the given mapper
func (cluster *MyCluster) QueryAll(ctx context.Context, query string, mapper RowMapper, args ...interface{}) ([][]interface{}, error) {
	values := make([][]interface{}, 0)
	for _, p := range cluster.partitions {
		results, err := p.QueryAll(ctx, query, mapper, args...)
		if err != nil {
			return nil, err
		}

		if results != nil {
			values = append(values, results)
		}
	}

	return values, nil
}

// GetAll get all entities from all partition by the given prototype
func (cluster *MyCluster) GetAll(ctx context.Context, prototype interface{}, query string, args ...interface{}) ([][]interface{}, error) {
	return cluster.QueryAll(ctx, query, NewSmartMapper(prototype), args...)
}

// QueryAllReduced query all entities and apply the reducer
func (cluster *MyCluster) QueryAllReduced(ctx context.Context, query string, mapper RowMapper, reducer func([][]interface{}) []interface{}, args ...interface{}) ([]interface{}, error) {
	results, err := cluster.QueryAll(ctx, query, mapper, args...)
	if err != nil {
		return nil, err
	}

	return reducer(results), nil
}

// GetAllReduced get all entries and apply the reduce
func (cluster *MyCluster) GetAllReduced(ctx context.Context, prototype interface{}, query string, reducer func([][]interface{}) []interface{}, args ...interface{}) ([]interface{}, error) {
	return cluster.QueryAllReduced(ctx, query, NewSmartMapper(prototype), reducer, args...)
}

// QueryPage query a page of entities by the given page merger
func (cluster *MyCluster) QueryPage(ctx context.Context, query string, mapper RowMapper, merger *PageMerger, args ...interface{}) ([]interface{}, error) {
	results, err := cluster.QueryAll(ctx, query, mapper, args...)
	if err != nil {
		return nil, err
	}

	return merger.Merge(results...), nil
}

// GetPage get a page of entities by the given prototype
func (cluster *MyCluster) GetPage(ctx context.Context, prototype interface{}, query string, merger *PageMerger, args ...interface{}) ([]interface{}, error) {
	return cluster.QueryPage(ctx, query, NewSmartMapper(prototype), merger, args...)
}
