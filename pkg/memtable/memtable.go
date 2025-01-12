package memtable

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/adammck/archive/pkg/types"
	"github.com/jonboulle/clockwork"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	defaultDB               = "archive"
	metaCollectionName      = "meta"
	metaActiveMemtableDocID = "active_memtable"
	blueMemtableName        = "blue"
	greenMemtableName       = "green"
)

type Memtable struct {
	mongoURL string
	mongo    *mongo.Database
	clock    clockwork.Clock
}

func New(mongoURL string, clock clockwork.Clock) *Memtable {
	return &Memtable{
		mongoURL: mongoURL,
		clock:    clock,
	}
}

func (mt *Memtable) Get(ctx context.Context, key string) (*types.Record, string, error) {
	c, err := mt.activeCollection(ctx)
	if err != nil {
		return nil, "", err
	}

	res := c.FindOne(ctx, bson.M{"key": key}, options.FindOne().SetSort(bson.M{"ts": -1}))

	b, err := res.Raw()
	if err != nil {

		// this is actually fine
		if err == mongo.ErrNoDocuments {
			return nil, "", nil
		}

		return nil, "", fmt.Errorf("FindOne: %w", err)
	}

	var rec types.Record
	err = bson.Unmarshal(b, &rec)
	if err != nil {
		return nil, "", fmt.Errorf("error decoding record: %w", err)
	}

	return &rec, mt.url(c.Name()), nil
}

func (mt *Memtable) url(coll string) string {
	u, err := url.Parse(mt.mongoURL)
	if err != nil {
		// extremely weird if this happens
		// TODO: move the parse to constructor
		return "error://error/error"
	}

	return fmt.Sprintf("%s://%s/%s/%s", u.Scheme, u.Host, mt.mongo.Name(), coll)
}

func (mt *Memtable) Put(ctx context.Context, key string, value []byte) (string, error) {
	c, err := mt.activeCollection(ctx)
	if err != nil {
		return "", err
	}

	_, err = c.InsertOne(ctx, &types.Record{
		Key:       key,
		Timestamp: mt.clock.Now(),
		Document:  value,
	})

	return mt.url(c.Name()), err
}

func (mt *Memtable) Ping(ctx context.Context) error {
	_, err := mt.GetMongo(ctx)
	return err
}

func (mt *Memtable) Init(ctx context.Context) error {
	db, err := mt.GetMongo(ctx)
	if err != nil {
		return fmt.Errorf("GetMongo: %w", err)
	}

	err = db.CreateCollection(ctx, metaCollectionName)
	if err != nil {
		return fmt.Errorf("CreateCollection: %w", err)
	}

	coll := db.Collection(metaCollectionName)
	_, err = coll.InsertOne(ctx, bson.M{
		"_id":   metaActiveMemtableDocID,
		"value": blueMemtableName,
	})
	if err != nil {
		return fmt.Errorf("InsertOne: %w", err)
	}

	// Initialize both memtables
	blue := NewHandle(db, blueMemtableName)
	if err := blue.Create(ctx); err != nil {
		return err
	}

	green := NewHandle(db, greenMemtableName)
	if err := green.Create(ctx); err != nil {
		return err
	}

	return nil
}

// TODO: Unexport this once flush is figured out.
func (mt *Memtable) GetMongo(ctx context.Context) (*mongo.Database, error) {
	if mt.mongo != nil {
		return mt.mongo, nil
	}

	m, err := connectToMongo(ctx, mt.mongoURL)
	if err != nil {
		return nil, err
	}

	mt.mongo = m
	return m, nil
}

func connectToMongo(ctx context.Context, url string) (*mongo.Database, error) {
	opt := options.Client().ApplyURI(url).SetTimeout(1 * time.Second)

	client, err := mongo.Connect(ctx, opt)
	if err != nil {
		return nil, err
	}

	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	return client.Database(defaultDB), nil
}

func (mt *Memtable) activeCollection(ctx context.Context) (*mongo.Collection, error) {
	m, err := mt.GetMongo(ctx)
	if err != nil {
		return nil, err
	}

	cn, err := mt.activeCollectionName(ctx, m)
	if err != nil {
		return nil, err
	}

	return m.Collection(cn), nil
}

func (mt *Memtable) activeCollectionName(ctx context.Context, db *mongo.Database) (string, error) {
	res := db.Collection(metaCollectionName).FindOne(ctx, bson.M{"_id": metaActiveMemtableDocID})

	var doc bson.M
	err := res.Decode(&doc)
	if err != nil {
		return "", fmt.Errorf("error decoding active memtable doc: %w", err)
	}

	val, ok := doc["value"]
	if !ok {
		return "", fmt.Errorf("no value key in active memtable doc: %#v", doc)
	}

	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("value in active memtable doc was not string, was: %T", val)
	}

	return s, nil
}

func (mt *Memtable) Swap(ctx context.Context) (*Handle, string, error) {
	db, err := mt.GetMongo(ctx)
	if err != nil {
		return nil, "", err
	}

	curr, err := mt.activeCollectionName(ctx, db)
	if err != nil {
		return nil, "", err
	}

	next := blueMemtableName
	if curr == blueMemtableName {
		next = greenMemtableName
	}

	_, err = db.Collection(metaCollectionName).UpdateOne(
		ctx,
		bson.M{"_id": metaActiveMemtableDocID},
		bson.M{"$set": bson.M{"value": next}},
	)
	if err != nil {
		return nil, "", fmt.Errorf("error updating active memtable: %w", err)
	}

	return NewHandle(db, curr), mt.url(next), nil
}
