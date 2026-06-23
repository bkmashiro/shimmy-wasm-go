pub fn is_correct(response: Option<&[u8]>, answer: Option<&[u8]>) -> bool {
    response.is_some() && answer.is_some() && response == answer
}

pub fn feedback<'a>(
    is_correct: bool,
    correct_feedback: Option<&'a [u8]>,
    incorrect_feedback: Option<&'a [u8]>,
) -> &'a [u8] {
    if is_correct {
        correct_feedback.unwrap_or(b"Correct!")
    } else {
        incorrect_feedback.unwrap_or(b"Try again.")
    }
}
