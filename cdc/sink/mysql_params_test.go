// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package sink

import (
	"context"
	"database/sql"
	"net/url"
	"strings"

	"github.com/DATA-DOG/go-sqlmock"
	dmysql "github.com/go-sql-driver/mysql"
	"github.com/pingcap/check"
	"github.com/pingcap/ticdc/pkg/util/testleak"
)

func (s MySQLSinkSuite) TestSinkParamsClone(c *check.C) {
	defer testleak.AfterTest(c)()
	param1 := defaultParams.Clone()
	param2 := param1.Clone()
	param2.changefeedID = "123"
	param2.batchReplaceEnabled = false
	param2.maxTxnRow = 1
	c.Assert(param1, check.DeepEquals, &sinkParams{
		workerCount:         DefaultWorkerCount,
		maxTxnRow:           DefaultMaxTxnRow,
		tidbTxnMode:         defaultTiDBTxnMode,
		batchReplaceEnabled: defaultBatchReplaceEnabled,
		batchReplaceSize:    defaultBatchReplaceSize,
		readTimeout:         defaultReadTimeout,
		writeTimeout:        defaultWriteTimeout,
		dialTimeout:         defaultDialTimeout,
		safeMode:            defaultSafeMode,
	})
	c.Assert(param2, check.DeepEquals, &sinkParams{
		changefeedID:        "123",
		workerCount:         DefaultWorkerCount,
		maxTxnRow:           1,
		tidbTxnMode:         defaultTiDBTxnMode,
		batchReplaceEnabled: false,
		batchReplaceSize:    defaultBatchReplaceSize,
		readTimeout:         defaultReadTimeout,
		writeTimeout:        defaultWriteTimeout,
		dialTimeout:         defaultDialTimeout,
		safeMode:            defaultSafeMode,
	})
}

func (s MySQLSinkSuite) TestGenerateDSNByParams(c *check.C) {
	defer testleak.AfterTest(c)()

	testDefaultParams := func() {
		db, err := mockTestDB()
		c.Assert(err, check.IsNil)
		defer db.Close()

		dsn, err := dmysql.ParseDSN("root:123456@tcp(127.0.0.1:4000)/")
		c.Assert(err, check.IsNil)
		params := defaultParams.Clone()
		dsnStr, err := generateDSNByParams(context.TODO(), dsn, params, db)
		c.Assert(err, check.IsNil)
		expectedParams := []string{
			"tidb_txn_mode=optimistic",
			"readTimeout=2m",
			"writeTimeout=2m",
			"allow_auto_random_explicit_insert=1",
		}
		for _, param := range expectedParams {
			c.Assert(strings.Contains(dsnStr, param), check.IsTrue)
		}
		c.Assert(strings.Contains(dsnStr, "time_zone"), check.IsFalse)
	}

	testTimezoneParam := func() {
		db, err := mockTestDB()
		c.Assert(err, check.IsNil)
		defer db.Close()

		dsn, err := dmysql.ParseDSN("root:123456@tcp(127.0.0.1:4000)/")
		c.Assert(err, check.IsNil)
		params := defaultParams.Clone()
		params.timezone = `"UTC"`
		dsnStr, err := generateDSNByParams(context.TODO(), dsn, params, db)
		c.Assert(err, check.IsNil)
		c.Assert(strings.Contains(dsnStr, "time_zone=%22UTC%22"), check.IsTrue)
	}

	testTimeoutParams := func() {
		db, err := mockTestDB()
		c.Assert(err, check.IsNil)
		defer db.Close()

		dsn, err := dmysql.ParseDSN("root:123456@tcp(127.0.0.1:4000)/")
		c.Assert(err, check.IsNil)
		uri, err := url.Parse("mysql://127.0.0.1:3306/?read-timeout=4m&write-timeout=5m&timeout=3m")
		c.Assert(err, check.IsNil)
		params, err := parseSinkURIToParams(context.TODO(), uri, map[string]string{})
		c.Assert(err, check.IsNil)
		dsnStr, err := generateDSNByParams(context.TODO(), dsn, params, db)
		c.Assert(err, check.IsNil)
		expectedParams := []string{
			"readTimeout=4m",
			"writeTimeout=5m",
			"timeout=3m",
		}
		for _, param := range expectedParams {
			c.Assert(strings.Contains(dsnStr, param), check.IsTrue)
		}
	}

	testDefaultParams()
	testTimezoneParam()
	testTimeoutParams()
}

func (s MySQLSinkSuite) TestParseSinkURIToParams(c *check.C) {
	defer testleak.AfterTest(c)()
	expected := defaultParams.Clone()
	expected.workerCount = 64
	expected.maxTxnRow = 20
	expected.batchReplaceEnabled = true
	expected.batchReplaceSize = 50
	expected.safeMode = true
	expected.timezone = `"UTC"`
	expected.changefeedID = "cf-id"
	expected.captureAddr = "127.0.0.1:8300"
	expected.tidbTxnMode = "pessimistic"
	uriStr := "mysql://127.0.0.1:3306/?worker-count=64&max-txn-row=20" +
		"&batch-replace-enable=true&batch-replace-size=50&safe-mode=true" +
		"&tidb-txn-mode=pessimistic"
	opts := map[string]string{
		OptChangefeedID: expected.changefeedID,
		OptCaptureAddr:  expected.captureAddr,
	}
	uri, err := url.Parse(uriStr)
	c.Assert(err, check.IsNil)
	params, err := parseSinkURIToParams(context.TODO(), uri, opts)
	c.Assert(err, check.IsNil)
	c.Assert(params, check.DeepEquals, expected)
}

func (s MySQLSinkSuite) TestParseSinkURITimezone(c *check.C) {
	defer testleak.AfterTest(c)()
	uris := []string{
		"mysql://127.0.0.1:3306/?time-zone=Asia/Shanghai&worker-count=32",
		"mysql://127.0.0.1:3306/?time-zone=&worker-count=32",
		"mysql://127.0.0.1:3306/?worker-count=32",
	}
	expected := []string{
		"\"Asia/Shanghai\"",
		"",
		"\"UTC\"",
	}
	ctx := context.TODO()
	opts := map[string]string{}
	for i, uriStr := range uris {
		uri, err := url.Parse(uriStr)
		c.Assert(err, check.IsNil)
		params, err := parseSinkURIToParams(ctx, uri, opts)
		c.Assert(err, check.IsNil)
		c.Assert(params.timezone, check.Equals, expected[i])
	}
}

func (s MySQLSinkSuite) TestParseSinkURIBadQueryString(c *check.C) {
	defer testleak.AfterTest(c)()
	uris := []string{
		"",
		"postgre://127.0.0.1:3306",
		"mysql://127.0.0.1:3306/?worker-count=not-number",
		"mysql://127.0.0.1:3306/?max-txn-row=not-number",
		"mysql://127.0.0.1:3306/?ssl-ca=only-ca-exists",
		"mysql://127.0.0.1:3306/?batch-replace-enable=not-bool",
		"mysql://127.0.0.1:3306/?batch-replace-enable=true&batch-replace-size=not-number",
		"mysql://127.0.0.1:3306/?safe-mode=not-bool",
	}
	ctx := context.TODO()
	opts := map[string]string{OptChangefeedID: "changefeed-01"}
	var uri *url.URL
	var err error
	for _, uriStr := range uris {
		if uriStr != "" {
			uri, err = url.Parse(uriStr)
			c.Assert(err, check.IsNil)
		} else {
			uri = nil
		}
		_, err = parseSinkURIToParams(ctx, uri, opts)
		c.Assert(err, check.NotNil)
	}
}

func (s MySQLSinkSuite) TestCheckTiDBVariable(c *check.C) {
	defer testleak.AfterTest(c)()
	db, mock, err := sqlmock.New()
	c.Assert(err, check.IsNil)
	defer db.Close() //nolint:errcheck
	columns := []string{"Variable_name", "Value"}

	mock.ExpectQuery("show session variables like 'allow_auto_random_explicit_insert';").WillReturnRows(
		sqlmock.NewRows(columns).AddRow("allow_auto_random_explicit_insert", "0"),
	)
	val, err := checkTiDBVariable(context.TODO(), db, "allow_auto_random_explicit_insert", "1")
	c.Assert(err, check.IsNil)
	c.Assert(val, check.Equals, "1")

	mock.ExpectQuery("show session variables like 'no_exist_variable';").WillReturnError(sql.ErrNoRows)
	val, err = checkTiDBVariable(context.TODO(), db, "no_exist_variable", "0")
	c.Assert(err, check.IsNil)
	c.Assert(val, check.Equals, "")

	mock.ExpectQuery("show session variables like 'version';").WillReturnError(sql.ErrConnDone)
	_, err = checkTiDBVariable(context.TODO(), db, "version", "5.7.25-TiDB-v4.0.0")
	c.Assert(err, check.ErrorMatches, ".*"+sql.ErrConnDone.Error())
}
