curl -n -T <(git archive $1) https://hpush.herokuapp.com/push/`hk app`
