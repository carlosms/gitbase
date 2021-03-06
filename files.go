package gitbase

import (
	"bytes"
	"io"

	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/filemode"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-mysql-server.v0/sql"
	"gopkg.in/src-d/go-mysql-server.v0/sql/expression"
	"gopkg.in/src-d/go-mysql-server.v0/sql/plan"
)

type filesTable struct{}

// FilesSchema is the schema for the files table.
var FilesSchema = sql.Schema{
	{Name: "repository_id", Type: sql.Text, Source: "files"},
	{Name: "file_path", Type: sql.Text, Source: "files"},
	{Name: "blob_hash", Type: sql.Text, Source: "files"},
	{Name: "tree_hash", Type: sql.Text, Source: "files"},
	{Name: "tree_entry_mode", Type: sql.Text, Source: "files"},
	{Name: "blob_content", Type: sql.Blob, Source: "files"},
	{Name: "blob_size", Type: sql.Int64, Source: "files"},
}

func newFilesTable() Indexable {
	return new(filesTable)
}

var _ sql.PushdownProjectionAndFiltersTable = (*filesTable)(nil)
var _ Squashable = (*filesTable)(nil)

func (filesTable) isGitbaseTable()      {}
func (filesTable) isSquashable()        {}
func (filesTable) Resolved() bool       { return true }
func (filesTable) Name() string         { return FilesTableName }
func (filesTable) Schema() sql.Schema   { return FilesSchema }
func (filesTable) Children() []sql.Node { return nil }

func (t *filesTable) TransformExpressionsUp(f sql.TransformExprFunc) (sql.Node, error) {
	return t, nil
}

func (t *filesTable) TransformUp(f sql.TransformNodeFunc) (sql.Node, error) {
	return f(t)
}

func (filesTable) RowIter(ctx *sql.Context) (sql.RowIter, error) {
	span, ctx := ctx.Span("gitbase.FilesTable")
	iter := &filesIter{readContent: true}

	repoIter, err := NewRowRepoIter(ctx, iter)
	if err != nil {
		span.Finish()
		return nil, err
	}

	return sql.NewSpanIter(span, repoIter), nil
}

func (filesTable) HandledFilters(filters []sql.Expression) []sql.Expression {
	return handledFilters(FilesTableName, FilesSchema, filters)
}

func (filesTable) WithProjectAndFilters(
	ctx *sql.Context,
	columns, filters []sql.Expression,
) (sql.RowIter, error) {
	span, ctx := ctx.Span("gitbase.FilesTable")
	iter, err := rowIterWithSelectors(
		ctx, FilesSchema, FilesTableName, filters, columns,
		[]string{"repository_id", "blob_hash", "file_path", "tree_hash"},
		func(ctx *sql.Context, selectors selectors, exprs []sql.Expression) (RowRepoIter, error) {
			repos, err := selectors.textValues("repository_id")
			if err != nil {
				return nil, err
			}

			treeHashes, err := selectors.textValues("tree_hash")
			if err != nil {
				return nil, err
			}

			blobHashes, err := selectors.textValues("blob_hash")
			if err != nil {
				return nil, err
			}

			filePaths, err := selectors.textValues("file_path")
			if err != nil {
				return nil, err
			}

			return &filesIter{
				repos:       repos,
				treeHashes:  stringsToHashes(treeHashes),
				blobHashes:  stringsToHashes(blobHashes),
				filePaths:   filePaths,
				readContent: shouldReadContent(columns),
			}, nil
		},
	)

	if err != nil {
		span.Finish()
		return nil, err
	}

	return sql.NewSpanIter(span, iter), nil
}

// IndexKeyValueIter implements the sql.Indexable interface.
func (*filesTable) IndexKeyValueIter(
	ctx *sql.Context,
	colNames []string,
) (sql.IndexKeyValueIter, error) {
	s, ok := ctx.Session.(*Session)
	if !ok || s == nil {
		return nil, ErrInvalidGitbaseSession.New(ctx.Session)
	}

	return newFilesKeyValueIter(s.Pool, colNames)
}

// WithProjectFiltersAndIndex implements sql.Indexable interface.
func (*filesTable) WithProjectFiltersAndIndex(
	ctx *sql.Context,
	columns, filters []sql.Expression,
	index sql.IndexValueIter,
) (sql.RowIter, error) {
	span, ctx := ctx.Span("gitbase.FilesTable.WithProjectFiltersAndIndex")
	s, ok := ctx.Session.(*Session)
	if !ok || s == nil {
		span.Finish()
		return nil, ErrInvalidGitbaseSession.New(ctx.Session)
	}

	session, err := getSession(ctx)
	if err != nil {
		return nil, err
	}

	var iter sql.RowIter = newFilesIndexIter(index, session.Pool, shouldReadContent(columns))
	if len(filters) > 0 {
		iter = plan.NewFilterIter(ctx, expression.JoinAnd(filters...), iter)
	}

	return sql.NewSpanIter(span, iter), nil
}

func (filesTable) String() string {
	return printTable(FilesTableName, FilesSchema)
}

type filesIter struct {
	repo     *Repository
	commits  object.CommitIter
	seen     map[plumbing.Hash]struct{}
	files    *object.FileIter
	treeHash plumbing.Hash

	readContent bool

	// selectors for faster filtering
	repos      []string
	filePaths  []string
	blobHashes []plumbing.Hash
	treeHashes []plumbing.Hash
}

func (i *filesIter) NewIterator(repo *Repository) (RowRepoIter, error) {
	var iter object.CommitIter
	if len(i.repos) == 0 || stringContains(i.repos, repo.ID) {
		var err error
		iter, err = repo.Repo.CommitObjects()
		if err != nil {
			return nil, err
		}
	}

	return &filesIter{
		repo:        repo,
		commits:     iter,
		seen:        make(map[plumbing.Hash]struct{}),
		readContent: i.readContent,
		filePaths:   i.filePaths,
		blobHashes:  i.blobHashes,
		treeHashes:  i.treeHashes,
	}, nil
}

func (i *filesIter) shouldVisitTree(hash plumbing.Hash) bool {
	if _, ok := i.seen[hash]; ok {
		return false
	}

	if len(i.treeHashes) > 0 && !hashContains(i.treeHashes, hash) {
		return false
	}

	return true
}

func (i *filesIter) shouldVisitFile(file *object.File) bool {
	if len(i.filePaths) > 0 && !stringContains(i.filePaths, file.Name) {
		return false
	}

	if len(i.blobHashes) > 0 && !hashContains(i.blobHashes, file.Blob.Hash) {
		return false
	}

	return true
}

func (i *filesIter) Next() (sql.Row, error) {
	if i.commits == nil {
		return nil, io.EOF
	}

	for {
		if i.files == nil {
			for {
				commit, err := i.commits.Next()
				if err != nil {
					return nil, err
				}

				if !i.shouldVisitTree(commit.TreeHash) {
					continue
				}

				i.treeHash = commit.TreeHash
				i.seen[commit.TreeHash] = struct{}{}

				if i.files, err = commit.Files(); err != nil {
					return nil, err
				}

				break
			}
		}

		f, err := i.files.Next()
		if err != nil {
			if err == io.EOF {
				i.files = nil
				continue
			}
		}

		if !i.shouldVisitFile(f) {
			continue
		}

		return fileToRow(i.repo.ID, i.treeHash, f, i.readContent)
	}
}

func (i *filesIter) Close() error {
	if i.commits != nil {
		i.commits.Close()
	}

	return nil
}

func fileToRow(
	repoID string,
	treeHash plumbing.Hash,
	file *object.File,
	readContent bool,
) (sql.Row, error) {
	content, err := blobContent(&file.Blob, readContent)
	if err != nil {
		return nil, err
	}

	return sql.NewRow(
		repoID,
		file.Name,
		file.Hash.String(),
		treeHash.String(),
		file.Mode.String(),
		content,
		file.Size,
	), nil
}

type fileIndexKey struct {
	Repository string
	Packfile   string
	Hash       string
	Offset     int64
	Name       string
	Mode       int64
	Tree       string
}

func (k *fileIndexKey) encode() ([]byte, error) {
	var buf bytes.Buffer
	writeString(&buf, k.Repository)
	if err := writeHash(&buf, k.Packfile); err != nil {
		return nil, err
	}

	writeBool(&buf, k.Offset >= 0)
	if k.Offset >= 0 {
		writeInt64(&buf, k.Offset)
	} else {
		if err := writeHash(&buf, k.Hash); err != nil {
			return nil, err
		}
	}

	writeString(&buf, k.Name)
	writeInt64(&buf, k.Mode)

	if err := writeHash(&buf, k.Tree); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (k *fileIndexKey) decode(data []byte) error {
	var buf = bytes.NewBuffer(data)
	var err error
	if k.Repository, err = readString(buf); err != nil {
		return err
	}

	if k.Packfile, err = readHash(buf); err != nil {
		return err
	}

	ok, err := readBool(buf)
	if err != nil {
		return err
	}

	if ok {
		if k.Offset, err = readInt64(buf); err != nil {
			return err
		}
		k.Hash = ""
	} else {
		if k.Hash, err = readHash(buf); err != nil {
			return err
		}
		k.Offset = -1
	}

	if k.Name, err = readString(buf); err != nil {
		return err
	}

	if k.Mode, err = readInt64(buf); err != nil {
		return err
	}

	if k.Tree, err = readHash(buf); err != nil {
		return err
	}

	return nil
}

type filesKeyValueIter struct {
	pool    *RepositoryPool
	repo    *Repository
	repos   *RepositoryIter
	commits object.CommitIter
	files   *object.FileIter
	commit  *object.Commit
	idx     *repositoryIndex
	columns []string
	seen    map[plumbing.Hash]struct{}
}

func newFilesKeyValueIter(pool *RepositoryPool, columns []string) (*filesKeyValueIter, error) {
	repos, err := pool.RepoIter()
	if err != nil {
		return nil, err
	}

	return &filesKeyValueIter{
		pool:    pool,
		repos:   repos,
		columns: columns,
	}, nil
}

func (i *filesKeyValueIter) Next() ([]interface{}, []byte, error) {
	for {
		if i.commits == nil {
			var err error
			i.repo, err = i.repos.Next()
			if err != nil {
				return nil, nil, err
			}

			i.seen = make(map[plumbing.Hash]struct{})

			i.commits, err = i.repo.Repo.CommitObjects()
			if err != nil {
				return nil, nil, err
			}

			repo := i.pool.repositories[i.repo.ID]
			i.idx, err = newRepositoryIndex(repo.path, repo.kind)
			if err != nil {
				return nil, nil, err
			}
		}

		if i.files == nil {
			var err error
			i.commit, err = i.commits.Next()
			if err != nil {
				if err == io.EOF {
					i.commits = nil
					continue
				}
				return nil, nil, err
			}

			if _, ok := i.seen[i.commit.TreeHash]; ok {
				continue
			}
			i.seen[i.commit.TreeHash] = struct{}{}

			i.files, err = i.commit.Files()
			if err != nil {
				return nil, nil, err
			}
		}

		f, err := i.files.Next()
		if err != nil {
			if err == io.EOF {
				i.files = nil
				continue
			}
		}

		offset, packfile, err := i.idx.find(f.Blob.Hash)
		if err != nil {
			return nil, nil, err
		}

		// only fill hash if the object is an unpacked object
		var hash string
		if offset < 0 {
			hash = f.Blob.Hash.String()
		}

		key, err := encodeIndexKey(&fileIndexKey{
			Repository: i.repo.ID,
			Packfile:   packfile.String(),
			Hash:       hash,
			Offset:     offset,
			Name:       f.Name,
			Tree:       i.commit.TreeHash.String(),
			Mode:       int64(f.Mode),
		})
		if err != nil {
			return nil, nil, err
		}

		row, err := fileToRow(i.repo.ID, i.commit.TreeHash, f, stringContains(i.columns, "blob_content"))
		if err != nil {
			return nil, nil, err
		}

		values, err := rowIndexValues(row, i.columns, FilesSchema)
		if err != nil {
			return nil, nil, err
		}

		return values, key, nil
	}
}

func (i *filesKeyValueIter) Close() error {
	if i.commits != nil {
		i.commits.Close()
	}

	if i.files != nil {
		i.files.Close()
	}

	return i.repos.Close()
}

type filesIndexIter struct {
	index       sql.IndexValueIter
	decoder     *objectDecoder
	readContent bool
}

func newFilesIndexIter(index sql.IndexValueIter, pool *RepositoryPool, readContent bool) *filesIndexIter {
	return &filesIndexIter{
		index:       index,
		decoder:     newObjectDecoder(pool),
		readContent: readContent,
	}
}

func (i *filesIndexIter) Next() (sql.Row, error) {
	var err error
	var data []byte
	defer closeIndexOnError(&err, i.index)

	data, err = i.index.Next()
	if err != nil {
		return nil, err
	}

	var key fileIndexKey
	if err := decodeIndexKey(data, &key); err != nil {
		return nil, err
	}

	obj, err := i.decoder.decode(
		key.Repository,
		plumbing.NewHash(key.Packfile),
		key.Offset,
		plumbing.NewHash(key.Hash),
	)
	if err != nil {
		return nil, err
	}

	blob, ok := obj.(*object.Blob)
	if !ok {
		return nil, ErrInvalidObjectType.New(obj, "*object.Blob")
	}

	file := &object.File{
		Blob: *blob,
		Name: key.Name,
		Mode: filemode.FileMode(key.Mode),
	}

	return fileToRow(key.Repository, plumbing.NewHash(key.Tree), file, i.readContent)
}

func (i *filesIndexIter) Close() error {
	if i.decoder != nil {
		if err := i.decoder.Close(); err != nil {
			_ = i.index.Close()
			return err
		}
	}

	return i.index.Close()
}
