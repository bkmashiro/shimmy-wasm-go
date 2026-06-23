package compare

func IsCorrect(response, answer string) bool {
	return response == answer
}

func Feedback(isCorrect bool, correctFeedback, incorrectFeedback string) string {
	if isCorrect {
		if correctFeedback != "" {
			return correctFeedback
		}
		return "Correct"
	}
	if incorrectFeedback != "" {
		return incorrectFeedback
	}
	return "Incorrect"
}
