package aiobfs

import (
	"math"
	"sync"
)

// knobs.go — обучение САМИХ ЧИСЕЛ маскировки, а не только выбора из готовых.
//
// Раньше потолок был здесь: пять профилей заданы в коде, обучение выбирало
// лучший из пяти и больше ничего не могло. Теперь у каждого профиля есть две
// непрерывные ручки, которые подстраиваются на ходу:
//
//	padding — сколько добивать пакет мусором: прямой размен «скорость против
//	          неотличимости». Меньше добивки — быстрее, но размеры пакетов
//	          ближе к настоящим размерам туннеля.
//	decoy   — как часто слать ложные пакеты: держит форму трафика в паузах,
//	          но стоит и полосы, и батареи.
//
// Почему ручки ограничены рамками профиля, а не свободны. Смысл маскировки —
// выглядеть правдоподобным медиапотоком. Если разрешить обучению любые
// значения, оно быстро выедет за пределы того, что бывает у настоящего
// аудиозвонка, и получится трафик, не похожий ни на что, — то есть ровно
// та уникальная подпись, от которой мы уходим. Поэтому рамки задаёт профиль,
// а обучение двигается ВНУТРИ них: всё ещё аудиозвонок, просто на тихом или
// разговорчивом краю нормы.
//
// Метод — REINFORCE с гауссовым исследованием: пробуем значение рядом с
// текущим, смотрим на награду, сдвигаем текущее в сторону того, что окупилось.
// Базовый уровень вычитается, иначе любое положительное вознаграждение
// толкало бы ручку в сторону последней пробы независимо от её качества.

const (
	knobCount      = 2 // padding, decoy
	knobIdxPadding = 0
	knobIdxDecoy   = 1

	knobSigma        = 0.15 // размах пробы вокруг текущего значения
	knobLearnRate    = 0.08
	knobBaselineRate = 0.05
	// Ручка живёт в [0,1] и отображается в рамки профиля.
	knobInit = 0.5
)

// knobSet — обучаемые параметры одного профиля.
type knobSet struct {
	theta    [knobCount]float64 // текущее «среднее» значение ручки
	sample   [knobCount]float64 // проба, действующая прямо сейчас
	baseline float64
	haveBase bool
}

// knobLearner держит ручки всех профилей.
type knobLearner struct {
	mu   sync.Mutex
	sets []knobSet
	rng  *pseudoRand
}

func newKnobLearner(numProfiles int, rng *pseudoRand) *knobLearner {
	sets := make([]knobSet, numProfiles)
	for i := range sets {
		for k := 0; k < knobCount; k++ {
			sets[i].theta[k] = knobInit
			sets[i].sample[k] = knobInit
		}
	}
	return &knobLearner{sets: sets, rng: rng}
}

// current отдаёт действующие значения ручек профиля.
func (l *knobLearner) current(profile int) (padding, decoy float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if profile < 0 || profile >= len(l.sets) {
		return knobInit, knobInit
	}
	return l.sets[profile].sample[knobIdxPadding], l.sets[profile].sample[knobIdxDecoy]
}

// observe учит ручки профиля на полученной награде и тянет новую пробу.
func (l *knobLearner) observe(profile int, reward float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if profile < 0 || profile >= len(l.sets) {
		return
	}
	s := &l.sets[profile]

	if s.haveBase {
		advantage := reward - s.baseline
		for k := 0; k < knobCount; k++ {
			// Градиент логарифма гауссовой плотности по среднему:
			// (проба − среднее)/σ². Умноженный на преимущество, он двигает
			// среднее туда, где награда оказалась выше обычной.
			grad := (s.sample[k] - s.theta[k]) / (knobSigma * knobSigma)
			s.theta[k] = clamp01(s.theta[k] + knobLearnRate*advantage*grad)
		}
		s.baseline += knobBaselineRate * (reward - s.baseline)
	} else {
		s.baseline = reward
		s.haveBase = true
	}

	// Новая проба на следующее окно.
	for k := 0; k < knobCount; k++ {
		s.sample[k] = clamp01(s.theta[k] + knobSigma*l.rng.gaussian())
	}
}

// export/import для памяти между запусками.
func (l *knobLearner) export() [][]float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]float64, len(l.sets))
	for i, s := range l.sets {
		out[i] = []float64{s.theta[knobIdxPadding], s.theta[knobIdxDecoy]}
	}
	return out
}

func (l *knobLearner) importFrom(saved [][]float64) bool {
	if len(saved) != len(l.sets) {
		return false
	}
	for _, row := range saved {
		if len(row) != knobCount {
			return false
		}
		for _, v := range row {
			if isNotFinite(v) || v < 0 || v > 1 {
				return false
			}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, row := range saved {
		for k := 0; k < knobCount; k++ {
			l.sets[i].theta[k] = row[k]
			l.sets[i].sample[k] = row[k]
		}
	}
	return true
}

// ── Отображение ручки в рамки профиля ────────────────────────────────────────

// paddingRange отдаёт границы, внутри которых обучению разрешено двигать
// добивку.
//
// Если профиль не объявил нижнюю границу, параметр считается ЗАДАННЫМ
// ЖЁСТКО и обучение его не трогает. Это важно: молча переопределять явно
// выставленное значение — значит ломать ожидания того, кто собрал свой
// профиль под конкретную задачу. Рамки — это осознанное разрешение
// подстраиваться, а не режим по умолчанию.
func paddingRange(p Profile) (low, high int) {
	high = p.PaddingMax
	low = p.PaddingMaxLow
	if low <= 0 || low > high {
		return high, high
	}
	return low, high
}

// decoyRange — то же для частоты ложных пакетов.
func decoyRange(p Profile) (low, high float64) {
	high = p.DecoyProbability
	low = p.DecoyProbabilityLow
	if low <= 0 || low > high {
		return high, high
	}
	return low, high
}

func effectivePaddingMax(p Profile, knob float64) int {
	low, high := paddingRange(p)
	return low + int(math.Round(clamp01(knob)*float64(high-low)))
}

func effectiveDecoyProbability(p Profile, knob float64) float64 {
	low, high := decoyRange(p)
	return low + clamp01(knob)*(high-low)
}
