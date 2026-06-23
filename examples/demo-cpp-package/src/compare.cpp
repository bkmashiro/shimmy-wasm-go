#include "compare.hpp"

static const char kCorrect[] = "Correct!";
static const char kIncorrect[] = "Try again.";

bool bytes_equal(TextView a, TextView b) {
  if (a.len != b.len) return false;
  for (usize i = 0; i < a.len; i++) {
    if (a.ptr[i] != b.ptr[i]) return false;
  }
  return true;
}

TextView feedback_for(bool is_correct, TextView correct_feedback, TextView incorrect_feedback) {
  if (is_correct) {
    if (correct_feedback.ptr != nullptr) return correct_feedback;
    return TextView{kCorrect, sizeof(kCorrect) - 1};
  }
  if (incorrect_feedback.ptr != nullptr) return incorrect_feedback;
  return TextView{kIncorrect, sizeof(kIncorrect) - 1};
}
