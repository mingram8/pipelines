// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package list contains types and methods for performing ListXXX operations. In
// particular, the package exports the Options struct, which can be used for
// applying listing, filtering and pagination logic.
package list

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	sq "github.com/Masterminds/squirrel"
	api "github.com/kubeflow/pipelines/backend/api/go_client"
	"github.com/kubeflow/pipelines/backend/src/apiserver/filter"
	"github.com/kubeflow/pipelines/backend/src/common/util"
)

// token represents a WHERE clause when making a ListXXX query. It can either
// represent a query for an initial set of results, in which page
// SortByFieldValue and KeyFieldValue are nil. If the latter fields are not nil,
// then token represents a query for a subsequent set of results (i.e., the next
// page of results), with the two values pointing to the first record in the
// next set of results.
type token struct {
	// SortByFieldName is the field name to use when sorting.
	SortByFieldName string
	// SortByFieldValue is the value of the sorted field of the next row to be
	// returned.
	SortByFieldValue interface{}
	// KeyFieldName is the name of the primary key for the model being queried.
	KeyFieldName string
	// KeyFieldValue is the value of the sorted field of the next row to be
	// returned.
	KeyFieldValue interface{}
	// IsDesc is true if the sorting order should be descending.
	IsDesc bool
	// Filter represents the filtering that should be applied in the query.
	Filter *filter.Filter
}

func (t *token) unmarshal(pageToken string) error {
	errorF := func(err error) error {
		return util.NewInvalidInputErrorWithDetails(err, "Invalid package token.")
	}
	b, err := base64.StdEncoding.DecodeString(pageToken)
	if err != nil {
		return errorF(err)
	}

	if err = json.Unmarshal(b, t); err != nil {
		return errorF(err)
	}

	return nil
}

func (t *token) marshal() (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", util.NewInternalServerError(err, "Failed to serialize page token.")
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Options represents options used when making a ListXXX query. In particular,
// it contains information on how to sort and filter results. It also
// encapsulates all the logic required for making the query for an initial set
// of results as well as subsequent pages of results.
type Options struct {
	PageSize int
	*token
}

// NewOptionsFromToken creates a new Options struct from the passed in token
// which represents the next page of results. An empty nextPageToken will result
// in an error.
func NewOptionsFromToken(nextPageToken string, pageSize int) (*Options, error) {
	if nextPageToken == "" {
		return nil, fmt.Errorf("cannot create list.Options from empty page token")
	}
	pageSize, err := validatePageSize(pageSize)
	if err != nil {
		return nil, err
	}

	t := &token{}
	if err := t.unmarshal(nextPageToken); err != nil {
		return nil, err
	}
	return &Options{PageSize: pageSize, token: t}, nil
}

// NewOptions creates a new Options struct for the given listable. It uses
// sorting and filtering criteria parsed from sortBy and filterProto
// respectively.
func NewOptions(listable Listable, pageSize int, sortBy string, filterProto *api.Filter) (*Options, error) {
	pageSize, err := validatePageSize(pageSize)
	if err != nil {
		return nil, err
	}

	token := &token{KeyFieldName: listable.PrimaryKeyColumnName()}

	// Ignore the case of the letter. Split query string by space.
	queryList := strings.Fields(strings.ToLower(sortBy))
	// Check the query string format.
	if len(queryList) > 2 || (len(queryList) == 2 && queryList[1] != "desc" && queryList[1] != "asc") {
		return nil, util.NewInvalidInputError(
			"Received invalid sort by format %q. Supported format: \"field_name\", \"field_name desc\", or \"field_name asc\"", sortBy)
	}

	token.SortByFieldName = listable.DefaultSortField()
	if len(queryList) > 0 {
		var err error
		n, ok := listable.APIToModelFieldMap()[queryList[0]]
		if !ok {
			return nil, util.NewInvalidInputError("Invalid sorting field: %q: %s", queryList[0], err)
		}
		token.SortByFieldName = n
	}

	if len(queryList) == 2 {
		token.IsDesc = queryList[1] == "desc"
	}

	// Filtering.
	if filterProto != nil {
		f, err := filter.NewWithKeyMap(filterProto, listable.APIToModelFieldMap())
		if err != nil {
			return nil, err
		}
		token.Filter = f
	}

	return &Options{PageSize: pageSize, token: token}, nil
}

// AddToSelect adds WHERE clauses with the sorting and filtering criteria in the
// Options o to the supplied SelectBuilder, and returns the new SelectBuilder
// containing these.
func (o *Options) AddToSelect(sqlBuilder sq.SelectBuilder) sq.SelectBuilder {
	// If next row's value is specified, set those values in the clause.
	if o.SortByFieldValue != nil && o.KeyFieldValue != nil {
		if o.IsDesc {
			sqlBuilder = sqlBuilder.
				Where(sq.Or{sq.Lt{o.SortByFieldName: o.SortByFieldValue},
					sq.And{sq.Eq{o.SortByFieldName: o.SortByFieldValue}, sq.LtOrEq{o.KeyFieldName: o.KeyFieldValue}}})
		} else {
			sqlBuilder = sqlBuilder.
				Where(sq.Or{sq.Gt{o.SortByFieldName: o.SortByFieldValue},
					sq.And{sq.Eq{o.SortByFieldName: o.SortByFieldValue}, sq.GtOrEq{o.KeyFieldName: o.KeyFieldValue}}})
		}
	}

	order := "ASC"
	if o.IsDesc {
		order = "DESC"
	}
	sqlBuilder = sqlBuilder.
		OrderBy(fmt.Sprintf("%v %v", o.SortByFieldName, order)).
		OrderBy(fmt.Sprintf("%v %v", o.KeyFieldName, order))

	// Add one more item than what is requested.
	sqlBuilder = sqlBuilder.Limit(uint64(o.PageSize + 1))

	if o.Filter != nil {
		sqlBuilder = o.Filter.AddToSelect(sqlBuilder)
	}

	return sqlBuilder
}

// Listable is an interface that should be implemented by any resource/model
// that wants to support listing queries.
type Listable interface {
	// PrimaryKeyColumnName returns the primary key for model.
	PrimaryKeyColumnName() string
	// DefaultSortField returns the default field name to be used when sorting list
	// query results.
	DefaultSortField() string
	// APIToModelFieldMap returns a map from field names in the API representation
	// of the model to its corresponding field name in the model itself.
	APIToModelFieldMap() map[string]string
}

// NextPageToken returns a string that can be used to fetch the subsequent set
// of results using the same listing options in o, starting with listable as the
// first record.
func (o *Options) NextPageToken(listable Listable) (string, error) {
	t, err := o.nextPageToken(listable)
	if err != nil {
		return "", err
	}
	return t.marshal()
}

func (o *Options) nextPageToken(listable Listable) (*token, error) {
	elem := reflect.ValueOf(listable).Elem()
	elemName := elem.Type().Name()

	sortByField := elem.FieldByName(o.SortByFieldName)
	if !sortByField.IsValid() {
		return nil, fmt.Errorf("cannot sort by field %q on type %q", o.SortByFieldName, elemName)
	}

	keyField := elem.FieldByName(listable.PrimaryKeyColumnName())
	if !keyField.IsValid() {
		return nil, fmt.Errorf("type %q does not have key field %q", elemName, o.KeyFieldName)
	}

	return &token{
		SortByFieldName:  o.SortByFieldName,
		SortByFieldValue: sortByField.Interface(),
		KeyFieldName:     listable.PrimaryKeyColumnName(),
		KeyFieldValue:    keyField.Interface(),
		IsDesc:           o.IsDesc,
	}, nil
}

const (
	defaultPageSize = 20
	maxPageSize     = 200
)

func validatePageSize(pageSize int) (int, error) {
	if pageSize < 0 {
		return 0, util.NewInvalidInputError("The page size should be greater than 0. Got %q", pageSize)
	}

	if pageSize == 0 {
		// Use default page size if not provided.
		return defaultPageSize, nil
	}

	if pageSize > maxPageSize {
		return maxPageSize, nil
	}

	return pageSize, nil
}
