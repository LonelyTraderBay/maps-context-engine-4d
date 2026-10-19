package worker

type Job struct { ID string; Attempts int }
type Queue struct { jobs []Job }
func (q *Queue) Push(job Job) { q.jobs = append(q.jobs, job) }
func (q *Queue) Pop() (Job, bool) { if len(q.jobs) == 0 { return Job{}, false }; job := q.jobs[0]; q.jobs = q.jobs[1:]; return job, true }
