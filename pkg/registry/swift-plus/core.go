package swiftplus

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	dcontext "github.com/docker/distribution/context"
	storagedriver "github.com/docker/distribution/registry/storage/driver"
	"github.com/docker/distribution/registry/storage/driver/base"
	"github.com/docker/distribution/registry/storage/driver/factory"
	"github.com/mattes/migrate"
	"github.com/mattes/migrate/database/postgres"
	bindata "github.com/mattes/migrate/source/go-bindata"

	//sql driver for postgres
	_ "github.com/lib/pq"
)

const (
	plusDriverName = "swift-plus"
	//files below this size will have their content stored in the database in
	//addition to Swift
	maxInlineSizeBytes = 256
)

var sqlMigrations = map[string]string{
	"001_initial.up.sql": `
		BEGIN;
		CREATE TABLE files (
			dirname    TEXT      NOT NULL,
			basename   TEXT      NOT NULL,
			size_bytes BIGINT    NOT NULL,
			mtime      TIMESTAMP NOT NULL,
			content    BYTEA,
			location   TEXT,
			PRIMARY KEY (dirname, basename)
		);
		CREATE TABLE segments (
			location   TEXT   NOT NULL,
			number     INT    NOT NULL,
			size_bytes BIGINT NOT NULL,
			hash       TEXT   NOT NULL,
			PRIMARY KEY (location, number)
		);
		COMMIT;
	`,
	"001_initial.down.sql": `
		BEGIN;
		DROP TABLE files;
		DROP TABLE segments;
		COMMIT;
	`,
}

func init() {
	factory.Register(plusDriverName, &driverFactory{})
}

// driverFactory implements the factory.StorageDriverFactory interface
type driverFactory struct{}

func (factory *driverFactory) Create(parameters map[string]interface{}) (storagedriver.StorageDriver, error) {
	return FromParameters(parameters)
}

type plusDriver struct {
	swift *swiftInterface
	db    *sql.DB
}

type baseEmbed struct {
	base.Base
}

// Driver is a storagedriver.StorageDriver implementation backed by Openstack Swift
// Objects will be stored in the provided container.
// Metadata will be stored in a PostgreSQL database.
type Driver struct {
	baseEmbed
}

// NewDriver constructs a new "swift-plus" Driver with the given Postgres
// and Openstack Swift credentials and container name.
func NewDriver(params Parameters) (*Driver, error) {
	si, err := newSwiftInterface(params)
	if err != nil {
		return nil, err
	}

	db, err := connectToPostgres(params.PostgresURI)
	if err != nil {
		return nil, err
	}
	err = initializeSchema(db)
	if err != nil {
		return nil, err
	}

	return &Driver{
		baseEmbed: baseEmbed{
			Base: base.Base{
				StorageDriver: &plusDriver{si, db},
			},
		},
	}, nil
}

func prependPrefix(prefix, fullPath string) string {
	if prefix == "" {
		return strings.Trim(fullPath, "/")
	}
	return prefix + "/" + strings.Trim(fullPath, "/")
}

//Chooses a new random string for fileInfo.Location.
func plusRandLocation() (string, error) {
	randomData := make([]byte, 8)
	_, err := rand.Read(randomData)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(randomData), nil
}

func setReportedPath(err error, path string) error {
	if _, ok := err.(storagedriver.PathNotFoundError); ok {
		return storagedriver.PathNotFoundError{Path: path}
	}
	return err
}

////////////////////////////////////////////////////////////////////////////////

var dbNotExistErrRx = regexp.MustCompile(`^pq: database "(.+?)" does not exist$`)

//connectToPostgres is like sql.Open(), but it also creates the database on the first run.
func connectToPostgres(uri string) (*sql.DB, error) {
	db, err := sql.Open("postgres", uri)
	if err == nil {
		//apparently the "database does not exist" error only occurs when trying to issue the first statement
		_, err = db.Exec("SELECT 1")
	}
	if err == nil {
		//database exists
		return db, nil
	}
	match := dbNotExistErrRx.FindStringSubmatch(err.Error())
	if match == nil {
		//unexpected error
		db.Close()
		return nil, err
	}
	dbName := match[1]

	//remove the database name from the connection URL
	dbURL, err := url.Parse(uri)
	if err != nil {
		db.Close()
		return nil, err
	}
	dbURL.Path = "/"
	db2, err := sql.Open("postgres", dbURL.String())
	if err != nil {
		db.Close()
		return nil, err
	}
	defer db2.Close()

	_, err = db2.Exec("CREATE DATABASE " + dbName)
	return db, err
}

func initializeSchema(db *sql.DB) error {
	//use the "go-bindata" driver for github.com/mattes/migrate, but without
	//actually using go-bindata (go-bindata stubbornly insists on making its
	//generated functions public, but I don't want to pollute the API)
	var assetNames []string
	for name := range sqlMigrations {
		assetNames = append(assetNames, name)
	}
	asset := func(name string) ([]byte, error) {
		data, ok := sqlMigrations[name]
		if ok {
			return []byte(data), nil
		}
		return nil, &os.PathError{Op: "open", Path: "<swift-plus>/builtin-sql/" + name, Err: errors.New("not found")}
	}

	sourceDriver, err := bindata.WithInstance(bindata.Resource(assetNames, asset))
	if err != nil {
		return err
	}
	dbDriver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("go-bindata", sourceDriver, "postgres", dbDriver)
	if err != nil {
		return err
	}
	err = m.Up()
	if err == migrate.ErrNoChange {
		//no idea why this is an error
		return nil
	}
	return err
}

////////////////////////////////////////////////////////////////////////////////

//fileInfo describes an entry in the `files` table of the SQL database.
type fileInfo struct {
	DirName    string
	BaseName   string
	SizeBytes  int64 //negative value signifies directory
	ModifiedAt time.Time
	Contents   []byte //nil for large files (when .Location != "")
	Location   string //empty for files stored in the DB, otherwise indicates the object name in Swift
}

func (p *plusDriver) readFileInfo(ctx context.Context, fullPath string) (fi fileInfo, err error) {
	fi.DirName = path.Dir(fullPath)
	fi.BaseName = path.Base(fullPath)
	err = p.db.QueryRowContext(
		ctx, "SELECT size_bytes, mtime, content, location FROM files WHERE dirname = $1 AND basename = $2", fi.DirName, fi.BaseName,
	).Scan(&fi.SizeBytes, &fi.ModifiedAt, &fi.Contents, &fi.Location)
	return
}

func (p *plusDriver) writeFileInfo(ctx context.Context, fi fileInfo) error {
	if fi.ModifiedAt.IsZero() {
		fi.ModifiedAt = time.Now()
	}
	_, err := p.db.ExecContext(ctx, `
			INSERT INTO files (dirname, basename, size_bytes, mtime, content, location) VALUES ($1,$2,$3,$4,$5,$6)
				ON CONFLICT (dirname, basename) DO
				UPDATE SET size_bytes = EXCLUDED.size_bytes, mtime = EXCLUDED.mtime, content = EXCLUDED.content, location = EXCLUDED.location
		`,
		fi.DirName, fi.BaseName, fi.SizeBytes, fi.ModifiedAt, fi.Contents, fi.Location,
	)
	if err != nil {
		return err
	}

	//create directories above this file if necessary
	return p.mkdirAll(ctx, fi.DirName)
}

func (p *plusDriver) mkdirAll(ctx context.Context, fullPath string) error {
	if fullPath == "/" || fullPath == "" {
		return nil
	}

	dirname := path.Dir(fullPath)
	basename := path.Base(fullPath)

	_, err := p.db.ExecContext(ctx, `
			INSERT INTO files (dirname, basename, size_bytes, mtime, content, location) VALUES ($1,$2,-1,NOW(),'','')
				ON CONFLICT (dirname, basename) DO NOTHING
		`, dirname, basename,
	)
	if err != nil {
		return err
	}

	return p.mkdirAll(ctx, dirname)
}

//implement the storagedriver.FileInfo interface
func (fi fileInfo) Path() string       { return path.Join(fi.DirName, fi.BaseName) }
func (fi fileInfo) Size() int64        { return fi.SizeBytes }
func (fi fileInfo) ModTime() time.Time { return fi.ModifiedAt }
func (fi fileInfo) IsDir() bool        { return fi.SizeBytes < 0 }

//ObjectPath returns where the blob (if any) for this file is stored in Swift.
func (fi fileInfo) ObjectPath() string {
	return fi.Location + "/content"
}

////////////////////////////////////////////////////////////////////////////////

type plusSegment struct {
	Prefix    string
	Location  string
	Number    uint64
	SizeBytes uint64
	Hash      string
}

func (s plusSegment) ObjectPath() string {
	return fmt.Sprintf("%s/%016d", prependPrefix(s.Prefix, s.Location), int(s.Number))
}

func (p *plusDriver) readSegmentInfo(ctx context.Context, location string) (result []plusSegment, err error) {
	if location == "" {
		return nil, nil
	}

	var rows *sql.Rows
	rows, err = p.db.QueryContext(ctx,
		`SELECT number, size_bytes, hash FROM segments WHERE location = $1 ORDER BY number`, location)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		segment := plusSegment{Prefix: p.swift.ObjectPrefix, Location: location}
		err = rows.Scan(&segment.Number, &segment.SizeBytes, &segment.Hash)
		if err != nil {
			return
		}
		result = append(result, segment)
	}
	return
}

////////////////////////////////////////////////////////////////////////////////

//Name implements the storagedriver.StorageDriver interface.
func (p *plusDriver) Name() string {
	return plusDriverName
}

//GetContent implements the storagedriver.StorageDriver interface.
func (p *plusDriver) GetContent(ctx dcontext.Context, fullPath string) ([]byte, error) {
	//try to retrieve file from the database
	fi, err := p.readFileInfo(ctx, fullPath)

	if err == sql.ErrNoRows || fi.IsDir() {
		return nil, storagedriver.PathNotFoundError{Path: fullPath}
	}
	if err != nil {
		return nil, err
	}
	if fi.SizeBytes == 0 {
		return nil, nil
	}
	if len(fi.Contents) > 0 {
		return fi.Contents, nil
	}

	//file exists, but contents are too big for the DB -> look in Swift
	reader, err := p.swift.Reader(ctx, prependPrefix(p.swift.ObjectPrefix, fi.ObjectPath()), 0)
	if err != nil {
		return nil, setReportedPath(err, fi.Path())
	}
	defer reader.Close()
	return ioutil.ReadAll(reader)
}

//PutContent implements the storagedriver.StorageDriver interface.
func (p *plusDriver) PutContent(ctx dcontext.Context, fullPath string, contents []byte) error {
	//if file exists already, remove its previous content from Swift
	fi, err := p.readFileInfo(ctx, fullPath)
	switch err {
	case nil:
		err := p.deleteBlobs(ctx, fi)
		if err != nil {
			return err
		}
	case sql.ErrNoRows:
		//file does not exist yet -- nothing to do
	default:
		return err
	}

	//insert file into database
	fi = fileInfo{
		DirName:   path.Dir(fullPath),
		BaseName:  path.Base(fullPath),
		SizeBytes: int64(len(contents)),
		Contents:  contents,
	}
	uploadToSwift := len(contents) > maxInlineSizeBytes
	if uploadToSwift {
		fi.Contents = nil
		var err error
		fi.Location, err = plusRandLocation()
		if err != nil {
			return err
		}
	}
	err = p.writeFileInfo(ctx, fi)
	if err != nil {
		return err
	}

	//upload file to Swift
	if !uploadToSwift {
		return nil
	}

	_, err = p.swift.Write(ctx, prependPrefix(p.swift.ObjectPrefix, fi.ObjectPath()), contents)
	return setReportedPath(err, fullPath)
}

//Reader implements the storagedriver.StorageDriver interface.
func (p *plusDriver) Reader(ctx dcontext.Context, fullPath string, offset int64) (io.ReadCloser, error) {
	fi, err := p.readFileInfo(ctx, fullPath)
	if err == sql.ErrNoRows || fi.IsDir() {
		return nil, storagedriver.PathNotFoundError{Path: fullPath}
	}
	if err != nil {
		return nil, err
	}

	//fast path: return empty reader without further queries if offset exceeds file size
	if offset > fi.SizeBytes {
		return ioutil.NopCloser(bytes.NewReader(nil)), nil
	}

	//return content from DB if possible
	if fi.Location == "" {
		data := fi.Contents
		if offset > 0 {
			if offset > int64(len(data)) {
				data = nil
			} else {
				data = data[offset:]
			}
		}
		return ioutil.NopCloser(bytes.NewReader(data)), nil
	}

	//query Swift if necessary
	r, err := p.swift.Reader(ctx, prependPrefix(p.swift.ObjectPrefix, fi.ObjectPath()), offset)
	return r, setReportedPath(err, fi.Path())
}

//Writer implements the storagedriver.StorageDriver interface.
func (p *plusDriver) Writer(ctx dcontext.Context, fullPath string, append bool) (w storagedriver.FileWriter, err error) {
	w, err = newPlusWriter(ctx, p, fullPath, append)
	if w != nil {
		w = newBufferedWriter(w, p.swift.ChunkSize)
	}
	return
}

//Stat implements the storagedriver.StorageDriver interface.
func (p *plusDriver) Stat(ctx dcontext.Context, fullPath string) (storagedriver.FileInfo, error) {
	//special case: health check looks at Stat("/") even though it's entirely bogus
	if fullPath == "/" {
		return fileInfo{
			DirName:    "/",
			BaseName:   "/",
			SizeBytes:  -1,
			ModifiedAt: time.Unix(0, 0),
		}, nil
	}

	fi, err := p.readFileInfo(ctx, fullPath)
	if err == sql.ErrNoRows {
		return nil, storagedriver.PathNotFoundError{Path: fullPath}
	}
	return fi, err
}

//List implements the storagedriver.StorageDriver interface.
func (p *plusDriver) List(ctx dcontext.Context, fullPath string) ([]string, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT basename FROM files WHERE dirname = $1`, fullPath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		result   []string
		basename string
	)
	for rows.Next() {
		err := rows.Scan(&basename)
		if err != nil {
			return nil, err
		}
		result = append(result, path.Join(fullPath, basename))
	}
	return result, nil
}

//Move implements the storagedriver.StorageDriver interface.
func (p *plusDriver) Move(ctx dcontext.Context, sourcePath string, destPath string) error {
	fi1, err := p.readFileInfo(ctx, sourcePath)
	if err == sql.ErrNoRows {
		return storagedriver.PathNotFoundError{Path: sourcePath}
	}

	//delete target file, if it exists
	fi2, err := p.readFileInfo(ctx, destPath)
	switch err {
	case nil:
		err := p.deleteDownwards(ctx, fi2)
		if err != nil {
			return err
		}
	case sql.ErrNoRows:
		//no file at target -- nothing to do
	default:
		return err
	}

	//move DB record (includes creation of missing directories above target, and
	//deletion of now-empty directories above source)
	_, err = p.db.ExecContext(ctx,
		`UPDATE files SET dirname = $1, basename = $2 WHERE dirname = $3 AND basename = $4`,
		path.Dir(destPath), path.Base(destPath), fi1.DirName, fi1.BaseName,
	)
	if err != nil {
		return err
	}

	//create missing directories above target
	return p.mkdirAll(ctx, path.Dir(destPath))
}

//Delete implements the storagedriver.StorageDriver interface.
func (p *plusDriver) Delete(ctx dcontext.Context, fullPath string) error {
	fi, err := p.readFileInfo(ctx, fullPath)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil //nothing to do
		}
		return err
	}

	return p.deleteDownwards(ctx, fi)
}

//deleteDownwards removes all files and directories below `fi` from the DB
func (p *plusDriver) deleteDownwards(ctx context.Context, fi fileInfo) error {
	//if file has content and/or segments in Swift, remove them as well
	err := p.deleteBlobs(ctx, fi)
	if err != nil {
		return err
	}

	//for directories, recurse into children
	if fi.IsDir() {
		rows, err := p.db.QueryContext(ctx, `
			SELECT basename, size_bytes, mtime, content, location FROM files WHERE dirname = $1
		`, fi.Path())
		if err != nil {
			return err
		}
		defer rows.Close()

		fiSub := fileInfo{DirName: fi.Path()}
		for rows.Next() {
			err = rows.Scan(&fiSub.BaseName, &fiSub.SizeBytes, &fiSub.ModifiedAt, &fiSub.Contents, &fiSub.Location)
			if err != nil {
				return err
			}
			err = p.deleteDownwards(ctx, fiSub)
			if err != nil {
				return err
			}
		}
	}

	//delete DB entry for this file/directory
	_, err = p.db.ExecContext(ctx, `DELETE FROM files WHERE dirname = $1 AND basename = $2`, fi.DirName, fi.BaseName)
	return err
}

//deleteBlobs removes all blobs and segments from Swift that are associated with this file.
func (p *plusDriver) deleteBlobs(ctx context.Context, fi fileInfo) error {
	if fi.Location == "" {
		return nil
	}
	return p.swift.DeleteAll(ctx, prependPrefix(p.swift.ObjectPrefix, fi.Location)+"/")
}

//URLFor implements the storagedriver.StorageDriver interface.
func (p *plusDriver) URLFor(ctx dcontext.Context, fullPath string, options map[string]interface{}) (string, error) {
	fi, err := p.readFileInfo(ctx, fullPath)
	if err == sql.ErrNoRows {
		return "", storagedriver.PathNotFoundError{Path: fullPath}
	}
	if err != nil {
		return "", err
	}

	//can only generate a temp URL for files that are stored in Swift
	if fi.Location == "" {
		return "", storagedriver.ErrUnsupportedMethod{}
	}
	return p.swift.MakeTempURL(ctx, prependPrefix(p.swift.ObjectPrefix, fi.ObjectPath()), options)
}

////////////////////////////////////////////////////////////////////////////////

//plusWriter is the storagedriver.FileWriter implementation used by the plusDriver.
type plusWriter struct {
	p         *plusDriver
	ctx       context.Context
	cancelled bool
	closed    bool
	committed bool
	fullPath  string
	location  string
	segments  []plusSegment
}

var (
	errCancelled = fmt.Errorf("already cancelled")
	errClosed    = fmt.Errorf("already closed")
	errCommitted = fmt.Errorf("already committed")
)

func newPlusWriter(ctx context.Context, p *plusDriver, fullPath string, appendFlag bool) (*plusWriter, error) {
	fi, err := p.readFileInfo(ctx, fullPath)
	exists := err != sql.ErrNoRows
	if exists && err != nil {
		return nil, err
	}

	//delete previous file unless we intend to append
	if exists && !appendFlag {
		err := p.deleteDownwards(ctx, fi)
		if err != nil {
			return nil, err
		}
		exists = false //we just deleted it
	}

	//choose new location when file is first created
	location := fi.Location
	if !exists || location == "" {
		location, err = plusRandLocation()
		if err != nil {
			return nil, err
		}
	}

	//find existing segments when appending to a file
	var segments []plusSegment
	if exists && appendFlag {
		segments, err = p.readSegmentInfo(ctx, location)
		if err != nil {
			return nil, err
		}
	}

	return &plusWriter{
		p:        p,
		ctx:      ctx,
		fullPath: fullPath,
		location: location,
		segments: segments,
	}, nil
}

func (w *plusWriter) Write(buf []byte) (int, error) {
	//choose segment number (this uses that the segments are always ordered)
	s := plusSegment{
		Prefix:    w.p.swift.ObjectPrefix,
		Location:  w.location,
		Number:    1,
		SizeBytes: uint64(len(buf)),
	}
	if len(w.segments) > 0 {
		s.Number = w.segments[len(w.segments)-1].Number + 1
	}

	//upload segment to Swift
	var err error
	s.Hash, err = w.p.swift.Write(w.ctx, s.ObjectPath(), buf)
	if err != nil {
		return 0, setReportedPath(err, w.fullPath)
	}

	//record uploaded segment
	w.segments = append(w.segments, s)
	_, err = w.p.db.ExecContext(w.ctx,
		`INSERT INTO segments (location, number, size_bytes, hash) VALUES ($1, $2, $3, $4)`,
		s.Location, s.Number, s.SizeBytes, s.Hash,
	)
	return len(buf), err
}

func (w *plusWriter) Size() (n int64) {
	for _, s := range w.segments {
		n += int64(s.SizeBytes)
	}
	return
}

func (w *plusWriter) Close() error {
	if w.closed {
		return errClosed
	}
	if !w.committed && !w.cancelled {
		return w.Commit()
	}
	return nil
}

func (w *plusWriter) Cancel() error {
	if w.closed {
		return errClosed
	}
	w.cancelled = true
	err := w.p.Delete(w.ctx, w.fullPath)
	w.segments = nil
	return err
}

func (w *plusWriter) Commit() error {
	if w.closed {
		return errClosed
	} else if w.cancelled {
		return errCancelled
	} else if w.committed {
		return errCommitted
	}

	fi := fileInfo{
		DirName:   path.Dir(w.fullPath),
		BaseName:  path.Base(w.fullPath),
		SizeBytes: w.Size(),
		Location:  w.location,
	}

	//save large file in Swift and in the DB
	err := w.p.swift.WriteSLO(w.ctx, prependPrefix(w.p.swift.ObjectPrefix, fi.ObjectPath()), w.segments)
	if err != nil {
		return err
	}
	err = w.p.writeFileInfo(w.ctx, fi)
	if err != nil {
		return err
	}
	w.committed = true
	return nil
}

////////////////////////////////////////////////////////////////////////////////

type bufferedWriter struct {
	fw storagedriver.FileWriter
	bw *bufio.Writer
}

func newBufferedWriter(fw storagedriver.FileWriter, chunkSize int) *bufferedWriter {
	return &bufferedWriter{
		fw: fw,
		bw: bufio.NewWriterSize(fw, chunkSize),
	}
}

func (w *bufferedWriter) Write(data []byte) (n int, err error) {
	return w.bw.Write(data)
}

func (w *bufferedWriter) Close() error {
	err := w.bw.Flush()
	if err == nil {
		err = w.fw.Close()
	}
	return err
}

func (w *bufferedWriter) Size() int64 {
	return w.fw.Size() + int64(w.bw.Buffered())
}

func (w *bufferedWriter) Cancel() error {
	err := w.bw.Flush()
	if err == nil {
		err = w.fw.Cancel()
	}
	return err
}

func (w *bufferedWriter) Commit() error {
	err := w.bw.Flush()
	if err == nil {
		err = w.fw.Commit()
	}
	return err
}
