package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/chchayin/speediance-cli/pkg/workout"
)

// FetchWorkouts returns completed sessions over the last `days` days (GOAL.md
// §9.2). The date window is [today+1-days, today+1) so "today" is always
// included, matching the Python client. A non-zero API code yields an empty
// list plus a stderr warning (not an error), again mirroring Python.
func (c *Client) FetchWorkouts(ctx context.Context, days int) ([]workout.Workout, error) {
	y, m, d := c.now().Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	end := today.AddDate(0, 0, 1)
	start := end.AddDate(0, 0, -days)
	return c.fetchRecords(ctx, start, end)
}

// FetchDay returns each session recorded on the given local calendar date, fully
// resolved to its type-correct detail. This is the engine behind the `today`
// command: the agent asks for "the workout(s) on this day" without knowing what
// kind they were, and the tool discovers each session's type from the list (which
// carries the authoritative numeric type AND course-category label alongside the
// trainingId — so dispatch is collision-proof, unlike resolving from a bare id)
// and fetches the right namespace directly, with no cross-namespace probing.
// Returns a non-nil (possibly empty) slice so JSON encodes as [].
func (c *Client) FetchDay(ctx context.Context, date time.Time) ([]workout.SessionDetail, error) {
	y, m, d := date.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	ws, err := c.fetchRecords(ctx, day, day.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	target := day.Format("2006-01-02")
	out := make([]workout.SessionDetail, 0, len(ws))
	for _, w := range ws {
		// The window can spill a neighbouring day; keep only the target date.
		if dt := w.Date(); dt == nil || *dt != target {
			continue
		}
		sd, err := c.ResolveSession(ctx, w.TrainingID, modeForType(w.SessionType, w.WorkoutType))
		if err != nil {
			return nil, err
		}
		out = append(out, *sd)
	}
	return out, nil
}

// fetchRecords pulls the userTrainingDataRecord list for [start, end) and parses
// it. A non-zero API code is a soft failure (empty list + warning), mirroring the
// Python client.
func (c *Client) fetchRecords(ctx context.Context, start, end time.Time) ([]workout.Workout, error) {
	path := "/mobile/v2/report/userTrainingDataRecord" +
		"?startDate=" + start.Format("2006-01-02") +
		"&endDate=" + end.Format("2006-01-02")
	env, err := c.GetJSON(ctx, path)
	if err != nil {
		return nil, err
	}
	if env.Code != 0 {
		c.logger.Warn("fetch_workouts failed", "message", env.Message)
		return nil, nil
	}
	return workout.ParseRecords(env.Data)
}

// DispatchMode selects which session namespace(s) ResolveSession consults.
type DispatchMode int

const (
	// Auto probes the program namespace first, then free, then official course.
	// It is the default for `session <id>` when the session type is unknown.
	Auto DispatchMode = iota
	// FreeFirst probes free first, then program, then official course. Used when
	// the list says the session is type 1, so the (rare) trainingId collision with
	// a program of the same id is sidestepped — the free session is found before
	// program is queried.
	FreeFirst
	// ProgramOnly consults only the program namespace and never falls back; it
	// backs the --program override for an id valid in multiple namespaces.
	ProgramOnly
	// FreeOnly consults only the free namespace and never falls back; it backs the
	// --free override for an id valid in multiple namespaces.
	FreeOnly
	// CourseOnly consults only the official-course namespace and never falls
	// back; it backs the --course override for an id valid in multiple
	// namespaces.
	CourseOnly
)

// modeForType picks a dispatch mode from the list record's session type and
// course-category label (workoutType — "Sculpting", "Strength Training",
// "Cardio", etc., or "" when absent). This routes straight to the one correct
// namespace instead of probing several and trusting whichever answers first:
// Speediance training IDs are not unique across the program/free/course tables,
// so probing risks silently attaching an unrelated session's data to the wrong
// id (confirmed in practice — a course-type id can coincidentally also exist as
// an unrelated free-lift/rowing record). Type 5 is always program/Coach,
// regardless of any category label. A non-empty category label with type != 5 is
// always an official built-in course. Only a genuinely blank type AND no
// category label — a true ad-hoc freestyle session, the one case with no
// distinguishing signal — falls back to probing (free first, since that is the
// overwhelmingly common real case; course/program are tried after only as a
// last-resort safety net, not because they are expected to answer).
func modeForType(sessionType int, workoutType string) DispatchMode {
	switch {
	case sessionType == 5:
		return ProgramOnly
	case workoutType != "":
		return CourseOnly
	default:
		return FreeFirst
	}
}

// ResolveSession fetches one session's detail, autonomously selecting the right
// Speediance namespace. Program (custom template/Coach) detail lives at
// cttTrainingInfo[Detail]; free-lift detail (incl. rowing/ski) lives at
// freeTraining[Detail]; official built-in course detail (e.g. a guided program
// selected on the machine, like "Back & Biceps Workout" or "Hip & Abs Core
// Training") lives at courseTrainingInfo[Detail] — a third namespace the original
// Python client supported (get_training_detail's 'course' branch) that this port
// initially missed, leaving such sessions' detail as null even though the
// endpoint works fine. A trainingId can be valid in more than one namespace as
// unrelated sessions, so the caller's mode disambiguates. The result is the
// uniform, verbatim SessionDetail (info/detail carry whichever namespace's raw
// payload answered; kind names it; both null when none has data).
func (c *Client) ResolveSession(ctx context.Context, trainingID int64, mode DispatchMode) (*workout.SessionDetail, error) {
	id := strconv.FormatInt(trainingID, 10)
	sd := &workout.SessionDetail{TrainingID: trainingID}

	tryProgram := func() (bool, error) {
		info, detail, err := c.programDetail(ctx, id)
		if err != nil {
			return false, err
		}
		if hasData(info) || hasData(detail) {
			sd.Kind, sd.Info, sd.Detail = "program", info, detail
			return true, nil
		}
		return false, nil
	}
	tryFree := func() (bool, error) {
		info, detail, err := c.freeDetail(ctx, id)
		if err != nil {
			return false, err
		}
		if hasData(info) || hasData(detail) {
			sd.Kind, sd.Info, sd.Detail = "free", info, detail
			return true, nil
		}
		return false, nil
	}
	tryCourse := func() (bool, error) {
		info, detail, err := c.courseDetail(ctx, id)
		if err != nil {
			return false, err
		}
		if hasData(info) || hasData(detail) {
			sd.Kind, sd.Info, sd.Detail = "course", info, detail
			return true, nil
		}
		return false, nil
	}

	var err error
	switch mode {
	case ProgramOnly:
		_, err = tryProgram()
	case FreeOnly:
		_, err = tryFree()
	case CourseOnly:
		_, err = tryCourse()
	case FreeFirst:
		// Only reached for a blind-guess id with no record (modeForType's
		// freestyle fallback). Course before free: observed accounts have many
		// more course sessions than genuine freestyle ones, and course ids have
		// not been seen to collide with real free-lift data, whereas free ids
		// have (a course-type id can coincidentally hit an unrelated free-lift
		// record) — so checking course first is both more often right and safer.
		err = firstOf(tryCourse, tryFree, tryProgram)
	default: // Auto / ProgramFirst — also only reached for a blind-guess id.
		err = firstOf(tryProgram, tryCourse, tryFree)
	}
	if err != nil {
		return nil, err
	}
	return sd, nil
}

// firstOf runs each candidate in order, stopping at the first that finds data
// (or errors).
func firstOf(candidates ...func() (bool, error)) error {
	for _, try := range candidates {
		found, err := try()
		if err != nil || found {
			return err
		}
	}
	return nil
}

// programDetail fetches the program (Coach/template) namespace payloads verbatim.
func (c *Client) programDetail(ctx context.Context, id string) (info, detail json.RawMessage, err error) {
	return c.pair(ctx,
		"/app/trainingInfo/cttTrainingInfo/"+id,
		"/app/trainingInfo/cttTrainingInfoDetail/"+id)
}

// freeDetail fetches the free-lift (freestyle/rowing/ski) namespace payloads verbatim.
func (c *Client) freeDetail(ctx context.Context, id string) (info, detail json.RawMessage, err error) {
	return c.pair(ctx,
		"/app/trainingInfo/freeTraining/"+id,
		"/app/trainingInfo/freeTrainingDetail/"+id)
}

// courseDetail fetches the official built-in course (guided program selected on
// the machine) namespace payloads verbatim. This mirrors the Python client's
// `get_training_detail(training_type='course')` branch, which this Go port had
// not carried over.
func (c *Client) courseDetail(ctx context.Context, id string) (info, detail json.RawMessage, err error) {
	return c.pair(ctx,
		"/app/trainingInfo/courseTrainingInfo/"+id,
		"/app/trainingInfo/courseTrainingInfoDetail/"+id)
}

// pair GETs an info path and a detail path and returns their verbatim data
// payloads (nil when the call returned a non-zero code or empty body).
func (c *Client) pair(ctx context.Context, infoPath, detailPath string) (info, detail json.RawMessage, err error) {
	ei, err := c.GetJSON(ctx, infoPath)
	if err != nil {
		return nil, nil, err
	}
	ed, err := c.GetJSON(ctx, detailPath)
	if err != nil {
		return nil, nil, err
	}
	if ei.Code == 0 {
		info = rawData(ei.Data)
	}
	if ed.Code == 0 {
		detail = rawData(ed.Data)
	}
	return info, detail, nil
}

// rawData returns the payload verbatim, or nil for an absent/empty one so it
// serializes as JSON null rather than invalid empty bytes. The payload itself is
// never altered.
func rawData(d json.RawMessage) json.RawMessage {
	if len(d) == 0 {
		return nil
	}
	return d
}

// hasData reports whether a payload carries something a session actually has —
// i.e. not absent, null, or an empty array/object. It decides whether a namespace
// "answered" so ResolveSession knows to stop or fall back.
func hasData(d json.RawMessage) bool {
	s := strings.TrimSpace(string(d))
	return s != "" && s != "null" && s != "[]" && s != "{}"
}
