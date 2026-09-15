package aiobfs

import (
	"encoding/json"
	"fmt"
	"sync"
)

// state.go — сохранение и восстановление выученного.
//
// Зачем. Без этого весь онлайн-опыт жил ровно до конца процесса: каждое
// переподключение начинало с равномерных весов и заново перебирало профили,
// в том числе заведомо плохие для этой сети. Между тем самое ценное здесь —
// именно накопленное знание «в этой сети такая маскировка проходит, а такая
// душится».
//
// Что сохраняем:
//   - веса бандита (какой профиль в среднем окупался);
//   - веса маленькой policy-сети (как выбор зависит от RTT, потерь и скорости);
//   - калибровку скорости (лучшая наблюдавшаяся) — иначе после перезапуска
//     награда за скорость какое-то время нормируется не на что.
//
// Чего НЕ сохраняем: ключ, адреса, объёмы трафика, вообще ничего, по чему
// можно восстановить, куда и что ходило. Только безразмерные веса.
//
// Формат — JSON с номером версии: набор профилей может поменяться, и тогда
// старые веса относятся к другим рукам бандита. Несовпадение числа профилей
// или версии = состояние молча игнорируется, обучение начинается с нуля.

const stateVersion = 1

// State — сериализуемый слепок выученного.
type State struct {
	Version     int         `json:"v"`
	NumProfiles int         `json:"n"`
	Bandit      []float64   `json:"bandit"`
	Baseline    float64     `json:"baseline"`
	PolicyWIn   [][]float64 `json:"p_win"`
	PolicyBIn   []float64   `json:"p_bin"`
	PolicyWOut  [][]float64 `json:"p_wout"`
	PolicyBOut  []float64   `json:"p_bout"`
	PolicyBase  float64     `json:"p_base"`
	BestTputBps float64     `json:"best_bps"`
}

// ExportState отдаёт слепок выученного, пригодный для записи на диск.
func (s *Shaper) ExportState() ([]byte, error) {
	s.learnMu.Lock()
	best := s.bestThroughput
	s.learnMu.Unlock()

	st := State{
		Version:     stateVersion,
		NumProfiles: len(s.profiles),
		BestTputBps: best,
	}
	s.bandit.exportInto(&st)
	s.policy.exportInto(&st)

	data, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("aiobfs: сериализация состояния: %w", err)
	}
	return data, nil
}

// ImportState восстанавливает выученное. Несовместимый слепок игнорируется
// без ошибки: начать с нуля всегда безопасно, а падать из-за устаревшего
// файла на диске — нет.
func (s *Shaper) ImportState(data []byte) (applied bool, err error) {
	if len(data) == 0 {
		return false, nil
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return false, fmt.Errorf("aiobfs: разбор состояния: %w", err)
	}
	if st.Version != stateVersion || st.NumProfiles != len(s.profiles) {
		return false, nil
	}
	if !s.bandit.importFrom(&st) {
		return false, nil
	}
	if !s.policy.importFrom(&st) {
		return false, nil
	}

	s.learnMu.Lock()
	if st.BestTputBps > 0 {
		s.bestThroughput = st.BestTputBps
	}
	s.learnMu.Unlock()
	return true, nil
}

func (e *exp3) exportInto(st *State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st.Bandit = append([]float64(nil), e.weights...)
	st.Baseline = e.baseline
}

func (e *exp3) importFrom(st *State) bool {
	if len(st.Bandit) != len(e.weights) {
		return false
	}
	for _, w := range st.Bandit {
		if w <= 0 || isNotFinite(w) {
			return false
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	copy(e.weights, st.Bandit)
	if st.Baseline > 0 && !isNotFinite(st.Baseline) {
		e.baseline = st.Baseline
	}
	return true
}

func (p *policyNet) exportInto(st *State) {
	st.PolicyWIn = cloneMatrix(p.wIn)
	st.PolicyBIn = append([]float64(nil), p.bIn...)
	st.PolicyWOut = cloneMatrix(p.wOut)
	st.PolicyBOut = append([]float64(nil), p.bOut...)
	st.PolicyBase = p.baseline
}

func (p *policyNet) importFrom(st *State) bool {
	if !sameShape(st.PolicyWIn, p.wIn) || len(st.PolicyBIn) != len(p.bIn) ||
		!sameShape(st.PolicyWOut, p.wOut) || len(st.PolicyBOut) != len(p.bOut) {
		return false
	}
	for _, row := range append(cloneMatrix(st.PolicyWIn), st.PolicyWOut...) {
		for _, v := range row {
			if isNotFinite(v) {
				return false
			}
		}
	}
	copyMatrix(p.wIn, st.PolicyWIn)
	copy(p.bIn, st.PolicyBIn)
	copyMatrix(p.wOut, st.PolicyWOut)
	copy(p.bOut, st.PolicyBOut)
	if !isNotFinite(st.PolicyBase) {
		p.baseline = st.PolicyBase
	}
	return true
}

func cloneMatrix(m [][]float64) [][]float64 {
	out := make([][]float64, len(m))
	for i, row := range m {
		out[i] = append([]float64(nil), row...)
	}
	return out
}

func copyMatrix(dst, src [][]float64) {
	for i := range dst {
		copy(dst[i], src[i])
	}
}

func sameShape(a, b [][]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
	}
	return true
}

func isNotFinite(v float64) bool {
	return v != v || v > 1e300 || v < -1e300
}

// stateMu защищает файл состояния от одновременной записи несколькими
// сессиями: шейпер у каждой свой, а файл общий.
var stateMu sync.Mutex

// LockState даёт вызывающему монопольный доступ к файлу состояния.
func LockState() func() {
	stateMu.Lock()
	return stateMu.Unlock
}
