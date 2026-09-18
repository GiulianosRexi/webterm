package resources

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrRemote envuelve los errores que devuelve el sistema externo, para
// distinguirlos de los nuestros.
var ErrRemote = errors.New("el sistema externo devolvió un error")

// prResponse está tipada contra la query de github.go.
type prResponse struct {
	Data struct {
		Repository *struct {
			NameWithOwner string `json:"nameWithOwner"`
			PullRequest   *struct {
				Number         int    `json:"number"`
				Title          string `json:"title"`
				URL            string `json:"url"`
				State          string `json:"state"`
				IsDraft        bool   `json:"isDraft"`
				Mergeable      string `json:"mergeable"`
				ReviewDecision string `json:"reviewDecision"`
				Author         *struct {
					Login string `json:"login"`
				} `json:"author"`
				ReviewThreads struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						IsResolved bool `json:"isResolved"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							// Puede venir null: los checks de un PR viejo expiran.
							StatusCheckRollup *struct {
								State    string `json:"state"`
								Contexts struct {
									TotalCount int `json:"totalCount"`
									Nodes      []struct {
										TypeName   string `json:"__typename"`
										Conclusion string `json:"conclusion"`
										Status     string `json:"status"`
										State      string `json:"state"`
									} `json:"nodes"`
								} `json:"contexts"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"errors"`
}

// checkFailures son las conclusiones de CheckRun y los estados de
// StatusContext que cuentan como falla. SKIPPED, NEUTRAL y CANCELLED no lo
// son: un check salteado no rompe nada.
// checkCategory clasifica un contexto de check con las mismas categorías que
// usa el resumen de GitHub.
//
// Los dos tipos de contexto se leen distinto: un CheckRun dice si terminó en
// status y cómo le fue en conclusion; un StatusContext tiene todo en state.
func checkCategory(typeName, conclusion, status, state string) string {
	if typeName == "StatusContext" {
		switch state {
		case "SUCCESS":
			return "success"
		case "FAILURE", "ERROR":
			return "failing"
		case "PENDING":
			return "pending"
		case "EXPECTED":
			// Un check requerido que todavía nadie reportó. GitHub lo cuenta
			// aparte de los que están corriendo, y la distinción importa:
			// este puede no llegar nunca.
			return "expected"
		}
		return "other"
	}

	// CheckRun: mientras no esté COMPLETED sigue corriendo, sin importar qué
	// diga conclusion.
	if status != "" && status != "COMPLETED" {
		return "pending"
	}
	switch conclusion {
	case "SUCCESS":
		return "success"
	case "SKIPPED":
		return "skipped"
	case "FAILURE", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED":
		return "failing"
	case "CANCELLED":
		return "cancelled"
	case "NEUTRAL", "STALE":
		return "neutral"
	case "":
		return "pending"
	}
	return "other"
}

// parsePRResponse mapea la respuesta de GraphQL a PRState.
//
// Mira la clave `errors` aunque haya datos: gh escribe el JSON de error en
// stdout con exit code propio, así que confiar solo en el exit code deja pasar
// errores con un body que parsea bien.
func parsePRResponse(body []byte) (*PRState, error) {
	var res prResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("respuesta ilegible de GitHub: %w", err)
	}
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrRemote, res.Errors[0].Message)
	}
	if res.Data.Repository == nil || res.Data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("%w: no se encontró el pull request", ErrRemote)
	}
	pr := res.Data.Repository.PullRequest

	out := &PRState{
		Number:         pr.Number,
		Repo:           res.Data.Repository.NameWithOwner,
		Title:          pr.Title,
		State:          pr.State,
		IsDraft:        pr.IsDraft,
		Mergeable:      pr.Mergeable,
		ReviewDecision: pr.ReviewDecision,
	}
	if pr.Author != nil {
		out.Author = pr.Author.Login
	}

	for _, t := range pr.ReviewThreads.Nodes {
		if !t.IsResolved {
			out.UnresolvedCount++
		}
	}
	// La query pide first:100; si hay más, la cuenta es un piso, no el total.
	out.ThreadsTruncated = pr.ReviewThreads.TotalCount > len(pr.ReviewThreads.Nodes)

	if len(pr.Commits.Nodes) > 0 {
		if rollup := pr.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
			out.ChecksState = rollup.State
			out.ChecksTotal = rollup.Contexts.TotalCount
			for _, c := range rollup.Contexts.Nodes {
				switch checkCategory(c.TypeName, c.Conclusion, c.Status, c.State) {
				case "success":
					out.ChecksSuccess++
				case "failing":
					out.ChecksFailing++
				case "pending":
					out.ChecksPending++
				case "expected":
					out.ChecksExpected++
				case "skipped":
					out.ChecksSkipped++
				case "cancelled":
					out.ChecksCancelled++
				case "neutral":
					out.ChecksNeutral++
				default:
					out.ChecksOther++
				}
			}
		}
	}
	return out, nil
}
