#pragma once

using usize = __SIZE_TYPE__;

struct TextView {
  const char *ptr;
  usize len;
};

bool bytes_equal(TextView a, TextView b);
TextView feedback_for(bool is_correct, TextView correct_feedback, TextView incorrect_feedback);
