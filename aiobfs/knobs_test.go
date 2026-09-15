package aiobfs

import (
	"testing"
)

// Ручка сдвигается туда, где награда выше.
//
// Проверяем честно, без подглядывания во внутренности: даём одному учителю
// награду, растущую с добивкой, другому — падающую, и смотрим, что ручки
// разъехались в РАЗНЫЕ стороны.
func TestKnobsFollowReward(t *testing.T) {
	rewardHighPadding := func(l *knobLearner) float64 {
		padding, _ := l.current(0)
		return padding
	}
	rewardLowPadding := func(l *knobLearner) float64 {
		padding, _ := l.current(0)
		return 1 - padding
	}

	high := newKnobLearner(1, newPseudoRand())
	low := newKnobLearner(1, newPseudoRand())
	for i := 0; i < 400; i++ {
		high.observe(0, rewardHighPadding(high))
		low.observe(0, rewardLowPadding(low))
	}

	hp, _ := high.current(0)
	lp, _ := low.current(0)
	if hp <= lp {
		t.Fatalf("ручки не разъехались по награде: за большую добивку %.3f, за малую %.3f", hp, lp)
	}
}

// Ручка не выходит за [0,1], а значения — за рамки профиля: маскировка
// должна оставаться правдоподобной, иначе получится трафик, не похожий ни
// на что, то есть ровно уникальная подпись.
func TestKnobsStayInsideProfileBounds(t *testing.T) {
	l := newKnobLearner(1, newPseudoRand())
	for i := 0; i < 2000; i++ {
		// Награда-пила: толкает ручку то вверх, то вниз изо всех сил.
		reward := 0.0
		if i%2 == 0 {
			reward = 1.0
		}
		l.observe(0, reward)
		padding, decoy := l.current(0)
		if padding < 0 || padding > 1 || decoy < 0 || decoy > 1 {
			t.Fatalf("ручка вышла за [0,1]: padding=%.3f decoy=%.3f", padding, decoy)
		}
	}

	for _, p := range StandardProfiles() {
		lowPad, highPad := paddingRange(p)
		lowDec, highDec := decoyRange(p)
		for _, knob := range []float64{0, 0.25, 0.5, 0.75, 1} {
			if v := effectivePaddingMax(p, knob); v < lowPad || v > highPad {
				t.Fatalf("%s: добивка %d вне [%d, %d]", p.Name, v, lowPad, highPad)
			}
			if v := effectiveDecoyProbability(p, knob); v < lowDec || v > highDec {
				t.Fatalf("%s: доля ложных %.4f вне [%.4f, %.4f]", p.Name, v, lowDec, highDec)
			}
		}
	}
}

// Границы обязаны быть заданы осмысленно: нижняя строго меньше верхней,
// иначе ручке просто негде двигаться и параметрическое обучение — фикция.
func TestStandardProfilesHaveUsableKnobRanges(t *testing.T) {
	for _, p := range StandardProfiles() {
		lowPad, highPad := paddingRange(p)
		if lowPad >= highPad {
			t.Fatalf("%s: добивке негде двигаться: [%d, %d]", p.Name, lowPad, highPad)
		}
		lowDec, highDec := decoyRange(p)
		if lowDec >= highDec {
			t.Fatalf("%s: доле ложных негде двигаться: [%.4f, %.4f]", p.Name, lowDec, highDec)
		}
	}
}

// Выученные параметры переживают перезапуск вместе с остальным.
func TestKnobsSurviveStateRoundTrip(t *testing.T) {
	trained, err := New(Config{Key: testKey()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 300; i++ {
		trained.Observe(40, 0, 30e6)
	}
	beforePad, beforeDecoy := trained.knobs.current(0)

	blob, err := trained.ExportState()
	if err != nil {
		t.Fatalf("ExportState: %v", err)
	}

	fresh, err := New(Config{Key: testKey()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := fresh.ImportState(blob); err != nil {
		t.Fatalf("ImportState: %v", err)
	}
	afterPad, afterDecoy := fresh.knobs.current(0)

	// После восстановления проба равна среднему, поэтому сравниваем с
	// точностью до размаха исследования.
	if diff := beforePad - afterPad; diff > knobSigma*3 || diff < -knobSigma*3 {
		t.Fatalf("ручка добивки не восстановилась: %.3f против %.3f", beforePad, afterPad)
	}
	if diff := beforeDecoy - afterDecoy; diff > knobSigma*3 || diff < -knobSigma*3 {
		t.Fatalf("ручка ложных пакетов не восстановилась: %.3f против %.3f", beforeDecoy, afterDecoy)
	}
}

// Память версии 1 (без ручек) должна читаться, а не выбрасываться: терять
// накопленные веса из-за добавления новой ручки — обидно.
func TestStateV1StillAccepted(t *testing.T) {
	s, err := New(Config{Key: testKey()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 50; i++ {
		s.Observe(50, 0, 10e6)
	}
	blob, err := s.ExportState()
	if err != nil {
		t.Fatalf("ExportState: %v", err)
	}

	// Понижаем версию и выкидываем ручки — получается файл старого формата.
	var st State
	if err := jsonUnmarshalForTest(blob, &st); err != nil {
		t.Fatalf("разбор: %v", err)
	}
	st.Version = 1
	st.Knobs = nil
	old, err := jsonMarshalForTest(st)
	if err != nil {
		t.Fatalf("сборка: %v", err)
	}

	fresh, err := New(Config{Key: testKey()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	applied, err := fresh.ImportState(old)
	if err != nil {
		t.Fatalf("ImportState: %v", err)
	}
	if !applied {
		t.Fatalf("память версии 1 отвергнута, пользователь потерял накопленное")
	}
}

// Профиль без объявленных рамок обучение не трогает: явно заданное значение
// должно оставаться ровно таким, каким его задали.
func TestKnobsLeaveFixedProfilesAlone(t *testing.T) {
	fixed := Profile{
		Name:             "fixed",
		PayloadTypes:     []uint8{96},
		PaddingMax:       64,
		DecoyProbability: 1.0,
	}
	for _, knob := range []float64{0, 0.3, 0.5, 0.9, 1} {
		if got := effectivePaddingMax(fixed, knob); got != 64 {
			t.Fatalf("добивка изменена без разрешения профиля: %d вместо 64", got)
		}
		if got := effectiveDecoyProbability(fixed, knob); got != 1.0 {
			t.Fatalf("доля ложных изменена без разрешения профиля: %.3f вместо 1.0", got)
		}
	}
}
