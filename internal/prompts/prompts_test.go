package prompts

import "testing"

func TestGetMeQuiz(t *testing.T) {
	if Get(Me) == "" {
		t.Fatal("me.txt пустой/отсутствует")
	}
	if Get(Quiz) == "" {
		t.Fatal("quiz.txt пустой/отсутствует")
	}
}
