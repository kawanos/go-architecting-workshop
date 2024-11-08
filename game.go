/*
Copyright 2023 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package game

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"encoding/json"

	"cloud.google.com/go/spanner"
	"github.com/go-playground/validator/v10"
	"github.com/go-redis/redis"
	"go.opentelemetry.io/otel"
	"google.golang.org/api/iterator"
)

type UserParams struct {
	UserID   string `validate:"required,max=36"`
	UserName string
}

type ItemParams struct {
	ItemID string `validate:"required,max=36"`
}

type dbClient struct {
	Sc    *spanner.Client
	Cache Cacher
}

type Caching struct {
	RedisClient *redis.Client
}

func (c *Caching) Get(key string) (string, error) {
	result, err := c.RedisClient.Get(key).Result()
	return result, err
}

func (c *Caching) Set(key string, data string) error {
	err := c.RedisClient.Set(key, data, 2*time.Second).Err()
	return err
}

// var _ Cacher = (*cache)(nil)
var validate = validator.New(validator.WithRequiredStructEnabled())

func NewClient(ctx context.Context, dbString string, c Cacher) (dbClient, error) {

	client, err := spanner.NewClient(ctx, dbString)
	if err != nil {
		return dbClient{}, err
	}

	return dbClient{
		Sc:    client,
		Cache: c,
	}, nil
}

// create a user
func (d dbClient) CreateUser(ctx context.Context, w io.Writer, u UserParams) error {

	ctx, mainSpan := otel.Tracer("main").Start(ctx, "CreateUser")
	defer mainSpan.End()

	if err := validate.Struct(u); err != nil {
		return err
	}

	ctx, txSpan := otel.Tracer("main").Start(ctx, "DML in transaction")

	_, err := d.Sc.ReadWriteTransactionWithOptions(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
		ctx, preparedSpan := otel.Tracer("main").Start(ctx, "PreparingStatement")
		sqlToUsers := `INSERT users (user_id, name, created_at, updated_at)
		  VALUES (@userID, @userName, @timestamp, @timestamp)`
		t := time.Now().Format("2006-01-02 15:04:05")
		params := map[string]interface{}{
			"userID":    u.UserID,
			"userName":  u.UserName,
			"timestamp": t,
		}
		stmtToUsers := spanner.Statement{
			SQL:    sqlToUsers,
			Params: params,
		}
		preparedSpan.End()

		ctx, UpdateSpan := otel.Tracer("main").Start(ctx, "UpdateRecord")
		_, err := txn.UpdateWithOptions(ctx, stmtToUsers, spanner.QueryOptions{RequestTag: "func=CreateUser,env=dev,action=insert"})
		UpdateSpan.End()
		if err != nil {
			return err
		}

		return nil
	}, spanner.TransactionOptions{TransactionTag: "func=CreateUser,env=dev"})

	txSpan.End()

	return err
}

/*
add item specified item_id to specific user
additionally show example how to use span of trace
*/
func (d dbClient) AddItemToUser(ctx context.Context, w io.Writer, u UserParams, i ItemParams) error {

	ctx, mainSpan := otel.Tracer("main").Start(ctx, "AddItemUser")
	defer mainSpan.End()

	if err := validate.Struct(u); err != nil {
		return err
	}

	_, err := d.Sc.ReadWriteTransactionWithOptions(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {

		sqlToUsers := `INSERT user_items (user_id, item_id, created_at, updated_at)
		  VALUES (@userID, @itemID, @timestamp, @timestamp)`
		t := time.Now().Format("2006-01-02 15:04:05")
		params := map[string]interface{}{
			"userID":    u.UserID,
			"itemId":    i.ItemID,
			"timestamp": t,
		}
		stmtToUsers := spanner.Statement{
			SQL:    sqlToUsers,
			Params: params,
		}
		rowCountToUsers, err := txn.Update(ctx, stmtToUsers)
		log.Printf("%d records has been updated\n", rowCountToUsers)
		if err != nil {
			return err
		}
		return nil
	}, spanner.TransactionOptions{TransactionTag: "func=AddItemToUser,env=dev"})

	return err
}

// get items the user has
func (d dbClient) UserItems(ctx context.Context, w io.Writer, userID string) ([]map[string]interface{}, error) {

	ctx, mainSpan := otel.Tracer("main").Start(ctx, "GetCache")
	key := fmt.Sprintf("UserItems_%s", userID)
	data, err := d.Cache.Get(key)
	mainSpan.End()

	if err != nil {
		log.Println(key, "Error", err)
	} else {
		_, span := otel.Tracer("main").Start(ctx, "JsonUnmarshal")
		results := []map[string]interface{}{}
		err := json.Unmarshal([]byte(data), &results)
		if err != nil {
			log.Println(err)
		}
		span.End()
		log.Println(key, "from cache")
		return results, nil
	}

	txn := d.Sc.ReadOnlyTransaction()
	defer txn.Close()
	sql := `select users.name,items.item_name,user_items.item_id
		from user_items join items on items.item_id = user_items.item_id join users on users.user_id = user_items.user_id
		where user_items.user_id = @user_id`
	stmt := spanner.Statement{
		SQL: sql,
		Params: map[string]interface{}{
			"user_id": userID,
		},
	}

	ctx, querySpan := otel.Tracer("main").Start(ctx, "txnQuery")
	iter := txn.QueryWithOptions(ctx, stmt, spanner.QueryOptions{RequestTag: "func=UserItems,env=dev,action=query"})
	defer iter.Stop()
	querySpan.End()

	ctx, getResultsSpan := otel.Tracer("main").Start(ctx, "readResults")

	baseItemSliceCap := 100

	results := make([]map[string]interface{}, 0, baseItemSliceCap)
	for {
		row, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return results, err
		}
		var userName string
		var itemNames string
		var itemIds string
		if err := row.Columns(&userName, &itemNames, &itemIds); err != nil {
			return results, err
		}

		results = append(results,
			map[string]interface{}{
				"user_name": userName,
				"item_name": itemNames,
				"item_id":   itemIds,
			})

	}
	getResultsSpan.End()

	_, setResultsSpan := otel.Tracer("main").Start(ctx, "setResults")
	jsonedResults, err := json.Marshal(results)
	if err != nil {
		return results, err
	}
	err = d.Cache.Set(key, string(jsonedResults))
	if err != nil {
		log.Println(err)
	}
	setResultsSpan.End()

	return results, nil
}
