package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/ClusterCockpit/cc-backend/auth"
	"github.com/ClusterCockpit/cc-backend/schema"
	sq "github.com/Masterminds/squirrel"
	"github.com/jmoiron/sqlx"
)

type JobRepository struct {
	DB *sqlx.DB
}

// Find executes a SQL query to find a specific batch job.
// The job is queried using the batch job id, the cluster name,
// and the start time of the job in UNIX epoch time seconds.
// It returns a pointer to a schema.Job data structure and an error variable.
// To check if no job was found test err == sql.ErrNoRows
func (r *JobRepository) Find(
	jobId *int64,
	cluster *string,
	startTime *int64) (*schema.Job, error) {

	qb := sq.Select(schema.JobColumns...).From("job").
		Where("job.job_id = ?", jobId)

	if cluster != nil {
		qb = qb.Where("job.cluster = ?", *cluster)
	}
	if startTime != nil {
		qb = qb.Where("job.start_time = ?", *startTime)
	}

	sqlQuery, args, err := qb.ToSql()
	if err != nil {
		return nil, err
	}

	job, err := schema.ScanJob(r.DB.QueryRowx(sqlQuery, args...))
	return job, err
}

// FindById executes a SQL query to find a specific batch job.
// The job is queried using the database id.
// It returns a pointer to a schema.Job data structure and an error variable.
// To check if no job was found test err == sql.ErrNoRows
func (r *JobRepository) FindById(
	jobId int64) (*schema.Job, error) {
	sqlQuery, args, err := sq.Select(schema.JobColumns...).
		From("job").Where("job.id = ?", jobId).ToSql()
	if err != nil {
		return nil, err
	}

	job, err := schema.ScanJob(r.DB.QueryRowx(sqlQuery, args...))
	return job, err
}

// Start inserts a new job in the table, returning the unique job ID.
// Statistics are not transfered!
func (r *JobRepository) Start(job *schema.JobMeta) (id int64, err error) {
	res, err := r.DB.NamedExec(`INSERT INTO job (
		job_id, user, project, cluster, `+"`partition`"+`, array_job_id, num_nodes, num_hwthreads, num_acc,
		exclusive, monitoring_status, smt, job_state, start_time, duration, resources, meta_data
	) VALUES (
		:job_id, :user, :project, :cluster, :partition, :array_job_id, :num_nodes, :num_hwthreads, :num_acc,
		:exclusive, :monitoring_status, :smt, :job_state, :start_time, :duration, :resources, :meta_data
	);`, job)
	if err != nil {
		return -1, err
	}

	return res.LastInsertId()
}

// Stop updates the job with the database id jobId using the provided arguments.
func (r *JobRepository) Stop(
	jobId int64,
	duration int32,
	state schema.JobState,
	monitoringStatus int32) (err error) {

	stmt := sq.Update("job").
		Set("job_state", state).
		Set("duration", duration).
		Set("monitoring_status", monitoringStatus).
		Where("job.id = ?", jobId)

	_, err = stmt.RunWith(r.DB).Exec()
	return
}

// CountJobs returns the number of jobs for the specified user (if a non-admin user is found in that context) and state.
// The counts are grouped by cluster.
func (r *JobRepository) CountJobs(ctx context.Context, state *schema.JobState) (map[string]int, error) {
	// q := sq.Select("count(*)").From("job")
	// if cluster != nil {
	// 	q = q.Where("job.cluster = ?", cluster)
	// }
	// if state != nil {
	// 	q = q.Where("job.job_state = ?", string(*state))
	// }

	// err = q.RunWith(r.DB).QueryRow().Scan(&count)
	// return

	q := sq.Select("job.cluster, count(*)").From("job").GroupBy("job.cluster")
	if state != nil {
		q = q.Where("job.job_state = ?", string(*state))
	}
	if user := auth.GetUser(ctx); user != nil && !user.HasRole(auth.RoleAdmin) {
		q = q.Where("job.user = ?", user.Username)
	}

	counts := map[string]int{}
	rows, err := q.RunWith(r.DB).Query()
	if err != nil {
		return nil, err
	}

	for rows.Next() {
		var cluster string
		var count int
		if err := rows.Scan(&cluster, &count); err != nil {
			return nil, err
		}

		counts[cluster] = count
	}

	return counts, nil
}

// func (r *JobRepository) Query(
// 	filters []*model.JobFilter,
// 	page *model.PageRequest,
// 	order *model.OrderByInput) ([]*schema.Job, int, error) {

// }

func (r *JobRepository) UpdateMonitoringStatus(job int64, monitoringStatus int32) (err error) {
	stmt := sq.Update("job").
		Set("monitoring_status", monitoringStatus).
		Where("job.id = ?", job)

	_, err = stmt.RunWith(r.DB).Exec()
	return
}

// Stop updates the job with the database id jobId using the provided arguments.
func (r *JobRepository) Archive(
	jobId int64,
	monitoringStatus int32,
	metricStats map[string]schema.JobStatistics) error {

	stmt := sq.Update("job").
		Set("monitoring_status", monitoringStatus).
		Where("job.id = ?", jobId)

	for metric, stats := range metricStats {
		switch metric {
		case "flops_any":
			stmt = stmt.Set("flops_any_avg", stats.Avg)
		case "mem_used":
			stmt = stmt.Set("mem_used_max", stats.Max)
		case "mem_bw":
			stmt = stmt.Set("mem_bw_avg", stats.Avg)
		case "load":
			stmt = stmt.Set("load_avg", stats.Avg)
		case "net_bw":
			stmt = stmt.Set("net_bw_avg", stats.Avg)
		case "file_bw":
			stmt = stmt.Set("file_bw_avg", stats.Avg)
		}
	}

	if _, err := stmt.RunWith(r.DB).Exec(); err != nil {
		return err
	}
	return nil
}

// Add the tag with id `tagId` to the job with the database id `jobId`.
func (r *JobRepository) AddTag(jobId int64, tagId int64) error {
	_, err := r.DB.Exec(`INSERT INTO jobtag (job_id, tag_id) VALUES (?, ?)`, jobId, tagId)
	return err
}

// CreateTag creates a new tag with the specified type and name and returns its database id.
func (r *JobRepository) CreateTag(tagType string, tagName string) (tagId int64, err error) {
	res, err := r.DB.Exec("INSERT INTO tag (tag_type, tag_name) VALUES ($1, $2)", tagType, tagName)
	if err != nil {
		return 0, err
	}

	return res.LastInsertId()
}

func (r *JobRepository) GetTags(user *string) (tags []schema.Tag, counts map[string]int, err error) {
	tags = make([]schema.Tag, 0, 100)
	xrows, err := r.DB.Queryx("SELECT * FROM tag")
	if err != nil {
		return nil, nil, err
	}

	for xrows.Next() {
		var t schema.Tag
		if err := xrows.StructScan(&t); err != nil {
			return nil, nil, err
		}
		tags = append(tags, t)
	}

	q := sq.Select("t.tag_name, count(jt.tag_id)").
		From("tag t").
		LeftJoin("jobtag jt ON t.id = jt.tag_id").
		GroupBy("t.tag_name")
	if user != nil {
		q = q.Where("jt.job_id IN (SELECT id FROM job WHERE job.user = ?)", *user)
	}

	rows, err := q.RunWith(r.DB).Query()
	if err != nil {
		return nil, nil, err
	}

	counts = make(map[string]int)

	for rows.Next() {
		var tagName string
		var count int
		err = rows.Scan(&tagName, &count)
		if err != nil {
			fmt.Println(err)
		}
		counts[tagName] = count
	}
	err = rows.Err()

	return
}

// AddTagOrCreate adds the tag with the specified type and name to the job with the database id `jobId`.
// If such a tag does not yet exist, it is created.
func (r *JobRepository) AddTagOrCreate(jobId int64, tagType string, tagName string) (tagId int64, err error) {
	tagId, exists := r.TagId(tagType, tagName)
	if !exists {
		tagId, err = r.CreateTag(tagType, tagName)
		if err != nil {
			return 0, err
		}
	}

	return tagId, r.AddTag(jobId, tagId)
}

// TagId returns the database id of the tag with the specified type and name.
func (r *JobRepository) TagId(tagType string, tagName string) (tagId int64, exists bool) {
	exists = true
	if err := sq.Select("id").From("tag").
		Where("tag.tag_type = ?", tagType).Where("tag.tag_name = ?", tagName).
		RunWith(r.DB).QueryRow().Scan(&tagId); err != nil {
		exists = false
	}
	return
}

var ErrNotFound = errors.New("no such job or user")

// FindJobOrUser returns a job database ID or a username if a job or user machtes the search term.
// As 0 is a valid job id, check if username is "" instead in order to check what machted.
// If nothing matches the search, `ErrNotFound` is returned.
func (r *JobRepository) FindJobOrUser(ctx context.Context, searchterm string) (job int64, username string, err error) {
	user := auth.GetUser(ctx)
	if id, err := strconv.Atoi(searchterm); err == nil {
		qb := sq.Select("job.id").From("job").Where("job.job_id = ?", id)
		if user != nil && !user.HasRole(auth.RoleAdmin) {
			qb = qb.Where("job.user = ?", user.Username)
		}

		err := qb.RunWith(r.DB).QueryRow().Scan(&job)
		if err != nil && err != sql.ErrNoRows {
			return 0, "", err
		} else if err == nil {
			return job, "", nil
		}
	}

	if user == nil || user.HasRole(auth.RoleAdmin) {
		err := sq.Select("job.user").Distinct().From("job").
			Where("job.user = ?", searchterm).
			RunWith(r.DB).QueryRow().Scan(&username)
		if err != nil && err != sql.ErrNoRows {
			return 0, "", err
		} else if err == nil {
			return 0, username, nil
		}
	}

	return 0, "", ErrNotFound
}
